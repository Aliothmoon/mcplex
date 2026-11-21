package version

import (
	"regexp"
	"testing"
)

func TestVersionIsValidSemver(t *testing.T) {
	re := regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
	if !re.MatchString(Version) {
		t.Fatalf("Version %q is not valid semver", Version)
	}
}
