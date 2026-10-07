package runtime

import "testing"

// An upstream build tag is "b11040". Atoi fails on the leading letter, and
// returning 0 made every upstream version compare equal - so ModelFabric could not
// tell b11040 from b10662 when neither carried a parsed LlamaBuild.
func TestUpstreamBuildTagsCompare(t *testing.T) {
	newerV, olderV := versionParts("b11040"), versionParts("b10662")
	if compareVersions(newerV, olderV) <= 0 {
		t.Errorf("b11040 did not compare newer than b10662: %v vs %v", newerV, olderV)
	}
	if compareVersions(versionParts("2.41.0"), versionParts("2.33.0")) <= 0 {
		t.Error("2.41.0 did not compare newer than 2.33.0")
	}
}

// A runtime that states its llama.cpp build is preferred over one that does
// not, rather than ranking an LM Studio version against an upstream build tag.
func TestVersionsFromDifferentPackagersAreNotRanked(t *testing.T) {
	stated := &Definition{Name: "upstream", LlamaBuild: 11040}
	silent := &Definition{Name: "lmstudio"}
	if !newer(stated, versionParts("b11040"), silent, versionParts("2.41.0")) {
		t.Error("the runtime that states its build should win")
	}
	if newer(silent, versionParts("2.41.0"), stated, versionParts("b11040")) {
		t.Error("the one with no build should not win on a packager version")
	}
}
