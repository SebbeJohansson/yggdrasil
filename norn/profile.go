package norn

import (
	"os"
	"regexp"
	"strings"
)

// ProfileMaxChars is where the profile starts to outweigh the inbox: a 1B
// model given a long page about the reader and three short lines about the
// messages writes about the reader.
const ProfileMaxChars = 1500

var (
	profileComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	blankLines     = regexp.MustCompile(`\n{3,}`)
)

// LoadProfile is the profile as the prompt should see it; empty when there
// is none. Read on every request, so editing the file needs no restart.
func LoadProfile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return CleanProfile(string(data))
}

// CleanProfile drops comments and runs of blank lines, and cuts the profile
// at ProfileMaxChars.
func CleanProfile(raw string) string {
	text := strings.TrimSpace(blankLines.ReplaceAllString(profileComment.ReplaceAllString(raw, ""), "\n\n"))
	if runes := []rune(text); len(runes) > ProfileMaxChars {
		return string(runes[:ProfileMaxChars])
	}
	return text
}
