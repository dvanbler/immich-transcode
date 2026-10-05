package immich

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestSpecPinned keeps the spec file, its pin (api/spec.mk), SpecVersion and
// the integration server image in agreement.
func TestSpecPinned(t *testing.T) {
	data, err := os.ReadFile("../../api/immich-openapi-specs.json")
	if err != nil {
		t.Fatal(err)
	}
	pinFile, err := os.ReadFile("../../api/spec.mk")
	if err != nil {
		t.Fatal(err)
	}
	pin := map[string]string{}
	for _, line := range strings.Split(string(pinFile), "\n") {
		if k, v, ok := strings.Cut(line, " := "); ok {
			pin[k] = strings.TrimSpace(v)
		}
	}

	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != pin["SPEC_SHA256"] {
		t.Errorf("spec sha256 %s does not match api/spec.mk %s; run `make spec`", got, pin["SPEC_SHA256"])
	}
	var spec struct {
		Info struct{ Version string } `json:"info"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Info.Version != SpecVersion || "v"+SpecVersion != pin["IMMICH_VERSION"] {
		t.Errorf("versions disagree: spec %s, SpecVersion %s, api/spec.mk %s",
			spec.Info.Version, SpecVersion, pin["IMMICH_VERSION"])
	}
	compose, err := os.ReadFile("../../test/integration/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`immich-server:(v\S+)`).FindSubmatch(compose)
	if m == nil || string(m[1]) != pin["IMMICH_VERSION"] {
		t.Errorf("integration server image %q, want %s", m, pin["IMMICH_VERSION"])
	}
}
