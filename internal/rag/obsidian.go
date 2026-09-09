package rag

import (
	"regexp"
	"strings"
	"unicode"
)

type Callout struct {
	Type    string `json:"tipo"`
	Title   string `json:"titulo"`
	Content string `json:"contenido"`
}

var (
	inlineTagRE   = regexp.MustCompile(`#[A-Za-zÀ-ɏ][A-Za-z0-9_À-ɏ/-]*`)
	calloutRE     = regexp.MustCompile(`^\s{0,3}>\s*\[!([A-Za-z-]+)\][+-]?\s*(.*)$`)
	blockAnchorRE = regexp.MustCompile(`(^|\s)\^([A-Za-z0-9][A-Za-z0-9-]*)\s*$`)
)

var accentFold = strings.NewReplacer(
	"á", "a", "à", "a", "ä", "a", "â", "a", "ã", "a", "å", "a",
	"é", "e", "è", "e", "ë", "e", "ê", "e",
	"í", "i", "ì", "i", "ï", "i", "î", "i",
	"ó", "o", "ò", "o", "ö", "o", "ô", "o", "õ", "o",
	"ú", "u", "ù", "u", "ü", "u", "û", "u",
	"ñ", "n", "ç", "c", "ý", "y", "ÿ", "y",
)

func NormalizeTag(value string) string {
	normalized := accentFold.Replace(strings.ToLower(strings.TrimSpace(value)))
	return strings.Trim(normalized, "/#-")
}

func stripCodeRegions(body string) string {
	lines := strings.Split(body, "\n")
	inside, marker := false, byte(0)
	for index, line := range lines {
		if _, fence, _, ok := fenceParts(line); ok {
			if !inside {
				inside, marker = true, fence[0]
			} else if fence[0] == marker {
				inside = false
			}
			lines[index] = ""
			continue
		}
		if inside {
			lines[index] = ""
		}
	}
	return strings.Join(lines, "\n")
}

func ExtractInlineTags(body string) []string {
	clean := stripCodeRegions(body)
	indices := inlineTagRE.FindAllStringIndex(clean, -1)
	result, seen := []string{}, map[string]bool{}
	for _, index := range indices {
		if index[0] > 0 {
			previous, _ := utf8LastRune(clean[:index[0]])
			if unicode.IsLetter(previous) || unicode.IsDigit(previous) || previous == '_' || previous == '&' {
				continue
			}
		}
		tag := NormalizeTag(clean[index[0]+1 : index[1]])
		if tag != "" && !seen[tag] {
			seen[tag] = true
			result = append(result, tag)
		}
	}
	return result
}

func utf8LastRune(value string) (rune, int) {
	runes := []rune(value)
	if len(runes) == 0 {
		return 0, 0
	}
	return runes[len(runes)-1], 1
}

func ExtractFrontmatterTags(metadata map[string]any) []string {
	raw, ok := metadata["tags"]
	if !ok {
		raw = metadata["etiquetas"]
	}
	candidates := []string{}
	switch value := raw.(type) {
	case string:
		candidates = regexp.MustCompile(`[,\s]+`).Split(value, -1)
	case []any:
		for _, item := range value {
			candidates = append(candidates, stringify(item))
		}
	case []string:
		candidates = append(candidates, value...)
	}
	result, seen := []string{}, map[string]bool{}
	for _, candidate := range candidates {
		tag := NormalizeTag(candidate)
		if tag != "" && !seen[tag] {
			seen[tag] = true
			result = append(result, tag)
		}
	}
	return result
}

func CollectTags(metadata map[string]any, body string) []string {
	combined := append(ExtractFrontmatterTags(metadata), ExtractInlineTags(body)...)
	result, seen := []string{}, map[string]bool{}
	for _, tag := range combined {
		if !seen[tag] {
			seen[tag] = true
			result = append(result, tag)
		}
	}
	return result
}

func ExtractCallouts(body string) []Callout {
	lines := strings.Split(stripCodeRegions(body), "\n")
	result := []Callout{}
	for index := 0; index < len(lines); {
		match := calloutRE.FindStringSubmatch(lines[index])
		if match == nil {
			index++
			continue
		}
		content := []string{}
		index++
		for index < len(lines) && quoteRE.MatchString(lines[index]) {
			line := quoteRE.ReplaceAllString(lines[index], "")
			content = append(content, strings.TrimPrefix(line, " "))
			index++
		}
		result = append(result, Callout{Type: strings.ToLower(strings.TrimSpace(match[1])), Title: strings.TrimSpace(match[2]), Content: strings.TrimSpace(strings.Join(content, "\n"))})
	}
	return result
}

func HasDeprecationCallout(callouts []Callout) bool {
	deprecated := map[string]bool{"caution": true, "danger": true, "failure": true, "bug": true, "deprecated": true}
	for _, callout := range callouts {
		if deprecated[callout.Type] {
			return true
		}
	}
	return false
}

func ExtractBlockAnchors(body string) map[string]string {
	anchors := map[string]string{}
	for _, line := range strings.Split(stripCodeRegions(body), "\n") {
		match := blockAnchorRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		anchor := strings.ToLower(match[2])
		if _, exists := anchors[anchor]; !exists {
			anchors[anchor] = strings.TrimSpace(blockAnchorRE.ReplaceAllString(line, ""))
		}
	}
	return anchors
}

func SearchableSupplement(tags []string, callouts []Callout) string {
	parts := []string{}
	if len(tags) > 0 {
		parts = append(parts, "Etiquetas: "+strings.Join(tags, ", "))
	}
	for _, callout := range callouts {
		heading := callout.Title
		if heading == "" {
			heading = callout.Type
		}
		parts = append(parts, "Aviso ("+callout.Type+"): "+heading)
	}
	return strings.Join(parts, "\n")
}
