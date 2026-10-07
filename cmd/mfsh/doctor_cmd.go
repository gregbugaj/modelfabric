package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/doctor"
	"github.com/gregbugaj/modelfabric/internal/runtime"
)

// `mfsh doctor` renders internal/doctor checks without starting or changing the node.

func doctorCmd(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "address of the local ModelFabric node")
	asJSON := fs.Bool("json", false, "print checks as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	checks := doctor.Run(doctor.Opts{
		Addr:         *addr,
		Version:      version,
		ConfigPath:   defaultConfigPath(),
		ModelsRoot:   defaultModelsRoot(),
		StateDir:     defaultStateDir(),
		Home:         fabricHome(),
		LogDir:       logDir(),
		NodeLogPath:  nodeLogPath(),
		RuntimesRoot: runtimesRoot,
		// Suppress discovery logs because the report includes its own diagnostics.
		Runtimes: func(cfg config.Config) []*runtime.Definition {
			return discoverRuntimes(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		},
	})

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(checks)
	}
	return printChecks(checks)
}

func printChecks(checks []doctor.Check) error {
	var ok, warn, fail int
	section := ""
	for _, c := range checks {
		if c.Section != section {
			if section != "" {
				fmt.Println()
			}
			section = c.Section
			fmt.Println(bold(section))
		}
		mark := dim("·")
		switch c.Status {
		case doctor.StatusOK:
			mark, ok = green("✓"), ok+1
		case doctor.StatusWarn:
			mark, warn = yellow("!"), warn+1
		case doctor.StatusFail:
			mark, fail = red("✗"), fail+1
		}
		fmt.Printf("  %s %s  %s\n", mark, padVisible(c.Name, 18), c.Detail)
		if c.Fix != "" && (c.Status == doctor.StatusWarn || c.Status == doctor.StatusFail) {
			fmt.Printf("    %s %s\n", dim("→"), c.Fix)
		}
	}
	fmt.Println()
	summary := fmt.Sprintf("%d ok", ok)
	if warn > 0 {
		summary += ", " + plural(warn, "warning")
	}
	if fail > 0 {
		summary += ", " + plural(fail, "problem")
	}
	switch {
	case fail > 0:
		fmt.Println(red("✗ ") + summary)
		return errors.New("doctor found problems")
	case warn > 0:
		fmt.Println(yellow("! ") + summary)
	default:
		fmt.Println(green("✓ ") + summary + " — ready")
	}
	return nil
}
