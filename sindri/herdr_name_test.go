package sindri

import (
	"regexp"
	"testing"
)

func TestCloudHerdrNamesAreValid(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	for _, dir := range []string{"/home/sebbe/projects/sebbejohansson-front", "/x/A Very Long Project Folder Name Indeed", "/x/123"} {
		for _, name := range []string{HerdrAgentName(dir), PrefixedHerdrName("cloud-", dir)} {
			if !valid.MatchString(name) {
				t.Errorf("%s gives %q", dir, name)
			}
		}
	}
}
