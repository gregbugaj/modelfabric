package catalog

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Read architecture, context length, parameter count, and model kind from GGUF
// metadata rather than inferring them from filenames.
//
// Layout: magic "GGUF", uint32 version, uint64 tensor_count,
// uint64 metadata_kv_count, then that many key/value pairs.

const ggufMagic = 0x46554747 // "GGUF" little-endian

const (
	ggufUint8 = iota
	ggufInt8
	ggufUint16
	ggufInt16
	ggufUint32
	ggufInt32
	ggufFloat32
	ggufBool
	ggufString
	ggufArray
	ggufUint64
	ggufInt64
	ggufFloat64
)

type ggufMeta struct {
	Architecture  string
	Name          string
	Basename      string
	SizeLabel     string
	ContextLength int
	ParamCount    uint64
	Embedding     bool
	// FileType is llama.cpp's quantization enum (general.file_type); -1 when
	// the header does not say.
	FileType int
	// NextNLayers is the number of multi-token-prediction draft layers the
	// model carries (Qwen3.8 ships one at blk.64.nextn.*). Non-zero means the
	// engine can speculate with the model's own draft head, no second model.
	NextNLayers int
	// BlockCount is the number of transformer layers; ExpertCount is > 0 for
	// mixture-of-experts models. Offload ratios are converted to layer
	// counts with these.
	BlockCount  int
	ExpertCount int
	// ReasoningEfforts lists levels accepted by the chat template. Generic engine
	// levels may raise template errors; absent template evidence means unknown.
	ReasoningEfforts []string
}

// effortTuple finds the levels a chat template validates against, as in
//
//	{%- if reasoning_effort not in ('xhigh', 'medium', 'low') %}
//
// The template is Jinja, not something ModelFabric can evaluate, so this reads the
// one construct templates use to reject a bad level. No match means the levels
// are unknown, which is reported as unknown rather than guessed.
var effortTuple = regexp.MustCompile(`reasoning_effort\s+not\s+in\s*\(([^)]*)\)`)

var quoted = regexp.MustCompile(`['"]([A-Za-z0-9_-]{1,24})['"]`)

func parseReasoningEfforts(template string) []string {
	m := effortTuple.FindStringSubmatch(template)
	if m == nil {
		return nil
	}
	var out []string
	for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
		if !slices.Contains(out, q[1]) {
			out = append(out, q[1])
		}
	}
	return out
}

// HubID derives the identifier tools are configured with, e.g.
// "qwen/qwen3.8-27b", from the model's own metadata:
//
//	general.basename   = Qwen_Qwen3.8   -- "_" separates publisher from model
//	general.size_label = 27B
//
// Returns "" when the metadata does not carry enough to be unambiguous, in
// which case the caller falls back to the path-derived key.
func (m *ggufMeta) HubID() string {
	base := strings.TrimSpace(m.Basename)
	if base == "" {
		return ""
	}
	id := strings.ToLower(base)
	// Only the first "_" is the publisher separator; later ones belong to the
	// model name itself.
	if i := strings.Index(id, "_"); i > 0 && i < len(id)-1 {
		id = id[:i] + "/" + id[i+1:]
	}
	if size := strings.ToLower(strings.TrimSpace(m.SizeLabel)); size != "" {
		id += "-" + size
	}
	return id
}

// readGGUF parses just the metadata header. It never reads tensor data, so the
// cost is independent of how large the model is.
func readGGUF(path string) (*ggufMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<16)

	var magic, version uint32
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return nil, err
	}
	if magic != ggufMagic {
		return nil, errors.New("not a GGUF file")
	}
	if err := binary.Read(r, binary.LittleEndian, &version); err != nil {
		return nil, err
	}
	if version < 2 || version > 3 {
		return nil, fmt.Errorf("unsupported GGUF version %d", version)
	}

	var tensorCount, kvCount uint64
	if err := binary.Read(r, binary.LittleEndian, &tensorCount); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &kvCount); err != nil {
		return nil, err
	}
	// A corrupt header could claim an absurd count; refuse rather than spin.
	if kvCount > 1<<20 {
		return nil, fmt.Errorf("implausible metadata count %d", kvCount)
	}

	meta := &ggufMeta{FileType: -1}
	for i := uint64(0); i < kvCount; i++ {
		key, err := readGGUFString(r)
		if err != nil {
			return meta, nil // partial metadata is still useful
		}
		var vtype uint32
		if err := binary.Read(r, binary.LittleEndian, &vtype); err != nil {
			return meta, nil
		}

		switch {
		case key == "general.architecture" && vtype == ggufString:
			meta.Architecture, err = readGGUFString(r)
		case key == "general.name" && vtype == ggufString:
			meta.Name, err = readGGUFString(r)
		case key == "general.basename" && vtype == ggufString:
			meta.Basename, err = readGGUFString(r)
		case key == "general.size_label" && vtype == ggufString:
			meta.SizeLabel, err = readGGUFString(r)
		case key == "general.file_type":
			var n uint64
			n, err = readGGUFUint(r, vtype)
			meta.FileType = int(n)
		case key == "general.parameter_count":
			var n uint64
			n, err = readGGUFUint(r, vtype)
			meta.ParamCount = n
		case strings.HasSuffix(key, ".context_length"):
			var n uint64
			n, err = readGGUFUint(r, vtype)
			meta.ContextLength = int(n)
		case strings.HasSuffix(key, ".block_count"):
			var n uint64
			n, err = readGGUFUint(r, vtype)
			meta.BlockCount = int(n)
		case strings.HasSuffix(key, ".expert_count"):
			var n uint64
			n, err = readGGUFUint(r, vtype)
			meta.ExpertCount = int(n)
		case strings.HasSuffix(key, ".nextn_predict_layers"):
			var n uint64
			n, err = readGGUFUint(r, vtype)
			meta.NextNLayers = int(n)
		case key == "tokenizer.chat_template" && vtype == ggufString:
			// Retain only reasoning levels; chat templates can occupy tens of kilobytes.
			var tpl string
			tpl, err = readGGUFString(r)
			meta.ReasoningEfforts = parseReasoningEfforts(tpl)
		case strings.HasSuffix(key, ".pooling_type"):
			// Only embedding models declare a pooling type.
			meta.Embedding = true
			err = skipGGUFValue(r, vtype)
		default:
			err = skipGGUFValue(r, vtype)
		}
		if err != nil {
			return meta, nil
		}
	}
	return meta, nil
}

func nonNegative(v int64, err error) (uint64, error) {
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("negative value %d where a count was expected", v)
	}
	return uint64(v), nil
}

func readGGUFString(r *bufio.Reader) (string, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > 1<<24 {
		return "", fmt.Errorf("implausible string length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

// readGGUFUint reads a GGUF integer as an unsigned value. Signed types are
// decoded as signed and rejected when negative: read as unsigned, an encoded
// -1 became 18446744073709551615 and was catalogued as a parameter or layer
// count.
func readGGUFUint(r *bufio.Reader, vtype uint32) (uint64, error) {
	switch vtype {
	case ggufUint8:
		var v uint8
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case ggufInt8:
		var v int8
		err := binary.Read(r, binary.LittleEndian, &v)
		return nonNegative(int64(v), err)
	case ggufUint16:
		var v uint16
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case ggufInt16:
		var v int16
		err := binary.Read(r, binary.LittleEndian, &v)
		return nonNegative(int64(v), err)
	case ggufUint32:
		var v uint32
		err := binary.Read(r, binary.LittleEndian, &v)
		return uint64(v), err
	case ggufInt32:
		var v int32
		err := binary.Read(r, binary.LittleEndian, &v)
		return nonNegative(int64(v), err)
	case ggufUint64:
		var v uint64
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case ggufInt64:
		var v int64
		err := binary.Read(r, binary.LittleEndian, &v)
		return nonNegative(v, err)
	default:
		return 0, skipGGUFValue(r, vtype)
	}
}

var scalarSize = map[uint32]int64{
	ggufUint8: 1, ggufInt8: 1, ggufBool: 1,
	ggufUint16: 2, ggufInt16: 2,
	ggufUint32: 4, ggufInt32: 4, ggufFloat32: 4,
	ggufUint64: 8, ggufInt64: 8, ggufFloat64: 8,
}

func skipGGUFValue(r *bufio.Reader, vtype uint32) error {
	if size, ok := scalarSize[vtype]; ok {
		_, err := io.CopyN(io.Discard, r, size)
		return err
	}
	switch vtype {
	case ggufString:
		_, err := readGGUFString(r)
		return err
	case ggufArray:
		var elemType uint32
		if err := binary.Read(r, binary.LittleEndian, &elemType); err != nil {
			return err
		}
		var count uint64
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return err
		}
		if size, ok := scalarSize[elemType]; ok {
			// Skip fixed-width arrays in one seek. Check multiplication overflow first;
			// a negative CopyN length would leave array data to be parsed as metadata.
			if count > uint64(math.MaxInt64)/uint64(size) {
				return fmt.Errorf("array of %d elements is not a real length", count)
			}
			_, err := io.CopyN(io.Discard, r, size*int64(count))
			return err
		}
		if elemType != ggufString {
			return fmt.Errorf("unsupported array element type %d", elemType)
		}
		for i := uint64(0); i < count; i++ {
			if _, err := readGGUFString(r); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported value type %d", vtype)
	}
}

func humanParams(n uint64) string {
	switch {
	case n == 0:
		return ""
	case n >= 1_000_000_000:
		v := float64(n) / 1e9
		if v >= 100 || v == float64(int(v)) {
			return fmt.Sprintf("%.0fB", v)
		}
		return fmt.Sprintf("%.1fB", v)
	case n >= 1_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// fileTypeNames maps llama.cpp's llama_ftype to the conventional labels, so a
// model's quantization comes from its header rather than whatever its file
// happens to be called.
var fileTypeNames = map[int]string{
	0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1", 7: "Q8_0", 8: "Q5_0", 9: "Q5_1",
	10: "Q2_K", 11: "Q3_K_S", 12: "Q3_K_M", 13: "Q3_K_L", 14: "Q4_K_S", 15: "Q4_K_M",
	16: "Q5_K_S", 17: "Q5_K_M", 18: "Q6_K", 19: "IQ2_XXS", 20: "IQ2_XS", 21: "Q2_K_S",
	22: "IQ3_XS", 23: "IQ3_XXS", 24: "IQ1_S", 25: "IQ4_NL", 26: "IQ3_S", 27: "IQ3_M",
	28: "IQ2_S", 29: "IQ2_M", 30: "IQ4_XS", 31: "IQ1_M", 32: "BF16", 36: "TQ1_0", 37: "TQ2_0",
}
