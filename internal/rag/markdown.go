package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type WikiLink struct {
	Target   string `json:"target"`
	Section  string `json:"section"`
	Alias    string `json:"alias"`
	Embedded bool   `json:"embedded"`
}

type MarkdownChunk struct {
	Index       int
	Heading     string
	Content     string
	ParentIndex *int
	HeadingPath []string
}

type MarkdownParent struct {
	Index            int
	Heading, Content string
	HeadingPath      []string
}

type OutlineEntry struct {
	Level   int      `json:"level"`
	Heading string   `json:"heading"`
	Path    []string `json:"path"`
}

type MarkdownSection struct {
	Heading, Content string
	Level            int
	Path             []string
}

type MarkdownDocument struct {
	Path       string
	Title      string
	Metadata   map[string]any
	Links      []WikiLink
	Chunks     []MarkdownChunk
	SHA256     string
	MTime      float64
	Parents    []MarkdownParent
	Tags       []string
	Supplement string
	Outline    []OutlineEntry
}

var (
	headingRE  = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+?)\s*$`)
	wikiLinkRE = regexp.MustCompile(`(!?)\[\[([^\]]+)\]\]`)
)

func CalculateSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func ParseFrontmatter(text string) (map[string]any, string) {
	normalized := strings.TrimPrefix(text, "\ufeff")
	lines := strings.Split(normalized, "\n")
	if len(lines) == 0 || strings.TrimSpace(strings.TrimSuffix(lines[0], "\r")) != "---" {
		return map[string]any{}, normalized
	}
	closing := -1
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(strings.TrimSuffix(lines[index], "\r")) == "---" {
			closing = index
			break
		}
	}
	if closing < 0 {
		return map[string]any{}, normalized
	}
	metadata := map[string]any{}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:closing], "\n")), &metadata); err != nil {
		metadata = map[string]any{"_frontmatter_error": "No se pudo interpretar el YAML"}
	}
	body := strings.Join(lines[closing+1:], "\n")
	body = strings.TrimPrefix(body, "\n")
	return metadata, body
}

func ExtractTitle(metadata map[string]any, body, fallback string) string {
	for _, key := range []string{"titulo", "title", "nombre"} {
		if value, ok := metadata[key]; ok && strings.TrimSpace(stringify(value)) != "" {
			return strings.TrimSpace(stringify(value))
		}
	}
	for _, match := range headingRE.FindAllStringSubmatch(body, -1) {
		if match[1] == "#" {
			return strings.TrimSpace(match[2])
		}
	}
	return fallback
}

func stripInlineCode(line string) string {
	runes := []rune(line)
	var output strings.Builder
	for index := 0; index < len(runes); {
		if runes[index] != '`' {
			output.WriteRune(runes[index])
			index++
			continue
		}
		count := 1
		for index+count < len(runes) && runes[index+count] == '`' {
			count++
		}
		closing := -1
		for cursor := index + count; cursor+count <= len(runes); cursor++ {
			found := true
			for offset := 0; offset < count; offset++ {
				if runes[cursor+offset] != '`' {
					found = false
					break
				}
			}
			if found {
				closing = cursor + count
				break
			}
		}
		if closing < 0 {
			output.WriteString(string(runes[index : index+count]))
			index += count
		} else {
			index = closing
		}
	}
	return output.String()
}

func stripMarkdownCode(body string) string {
	lines := strings.Split(body, "\n")
	inside, marker := false, byte(0)
	for index, line := range lines {
		if _, fence, _, ok := fenceParts(line); ok {
			if !inside {
				inside, marker = true, fence[0]
			} else if marker == fence[0] {
				inside = false
			}
			lines[index] = ""
		} else if inside {
			lines[index] = ""
		} else {
			lines[index] = stripInlineCode(line)
		}
	}
	return strings.Join(lines, "\n")
}

func ExtractWikiLinks(body string) []WikiLink {
	result, seen := []WikiLink{}, map[string]bool{}
	for _, match := range wikiLinkRE.FindAllStringSubmatch(stripMarkdownCode(body), -1) {
		raw := strings.ReplaceAll(strings.TrimSpace(match[2]), `\|`, "|")
		targetPart, alias, _ := strings.Cut(raw, "|")
		target, section, _ := strings.Cut(strings.TrimSpace(targetPart), "#")
		link := WikiLink{Target: strings.TrimSpace(target), Section: strings.TrimSpace(section), Alias: strings.TrimSpace(alias), Embedded: match[1] != ""}
		if link.Target == "" {
			continue
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%t", link.Target, link.Section, link.Alias, link.Embedded)
		if !seen[key] {
			seen[key] = true
			result = append(result, link)
		}
	}
	return result
}

type sectionMatch struct {
	start, end int
	heading    string
}

func SplitSections(body string) [][2]string {
	structured := SplitSectionsWithHierarchy(body)
	result := make([][2]string, 0, len(structured))
	for _, section := range structured {
		result = append(result, [2]string{section.Heading, section.Content})
	}
	return result
}

func SplitSectionsWithHierarchy(body string) []MarkdownSection {
	indices := headingRE.FindAllStringSubmatchIndex(body, -1)
	if len(indices) == 0 {
		if clean := strings.TrimSpace(body); clean != "" {
			return []MarkdownSection{{Heading: "Documento", Content: clean, Path: []string{"Documento"}}}
		}
		return nil
	}
	type hierarchicalMatch struct {
		sectionMatch
		level int
		path  []string
	}
	matches := make([]hierarchicalMatch, 0, len(indices))
	stack := []string{}
	for _, index := range indices {
		level := index[3] - index[2]
		heading := strings.TrimSpace(body[index[4]:index[5]])
		if len(stack) >= level {
			stack = stack[:level-1]
		}
		stack = append(stack, heading)
		path := append([]string(nil), stack...)
		matches = append(matches, hierarchicalMatch{sectionMatch: sectionMatch{start: index[0], end: index[1], heading: heading}, level: level, path: path})
	}
	sections := []MarkdownSection{}
	if intro := strings.TrimSpace(body[:matches[0].start]); intro != "" {
		sections = append(sections, MarkdownSection{Heading: "Introducción", Content: intro, Path: []string{"Introducción"}})
	}
	for index, match := range matches {
		end := len(body)
		if index+1 < len(matches) {
			end = matches[index+1].start
		}
		if content := strings.TrimSpace(body[match.end:end]); content != "" {
			sections = append(sections, MarkdownSection{Heading: match.heading, Content: content, Level: match.level, Path: match.path})
		}
	}
	return sections
}

func ExtractOutline(body string) []OutlineEntry {
	indices := headingRE.FindAllStringSubmatchIndex(body, -1)
	result, stack := make([]OutlineEntry, 0, len(indices)), []string{}
	for _, index := range indices {
		level := index[3] - index[2]
		heading := strings.TrimSpace(body[index[4]:index[5]])
		if len(stack) >= level {
			stack = stack[:level-1]
		}
		stack = append(stack, heading)
		result = append(result, OutlineEntry{Level: level, Heading: heading, Path: append([]string(nil), stack...)})
	}
	return result
}

type ParseOptions struct {
	ChunkSize, ChunkOverlap                            int
	ParentChildEnabled                                 bool
	ParentChunkSize, ChildChunkSize, ChildChunkOverlap int
}

func ParseMarkdownFile(path string, options ParseOptions) (MarkdownDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MarkdownDocument{}, err
	}
	text := strings.ToValidUTF8(string(data), "�")
	metadata, body := ParseFrontmatter(text)
	title := ExtractTitle(metadata, body, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	links := ExtractWikiLinks(body)
	chunks, parents, chunkIndex, parentIndex := []MarkdownChunk{}, []MarkdownParent{}, 0, 0
	for _, section := range SplitSectionsWithHierarchy(body) {
		if options.ParentChildEnabled {
			for _, parentContent := range SplitStructuredText(section.Content, options.ParentChunkSize, 0) {
				parents = append(parents, MarkdownParent{Index: parentIndex, Heading: section.Heading, Content: parentContent, HeadingPath: append([]string(nil), section.Path...)})
				for _, part := range SplitStructuredText(parentContent, options.ChildChunkSize, options.ChildChunkOverlap) {
					value := parentIndex
					chunks = append(chunks, MarkdownChunk{Index: chunkIndex, Heading: section.Heading, Content: part, ParentIndex: &value, HeadingPath: append([]string(nil), section.Path...)})
					chunkIndex++
				}
				parentIndex++
			}
		} else {
			for _, part := range SplitStructuredText(section.Content, options.ChunkSize, options.ChunkOverlap) {
				chunks = append(chunks, MarkdownChunk{Index: chunkIndex, Heading: section.Heading, Content: part, HeadingPath: append([]string(nil), section.Path...)})
				chunkIndex++
			}
		}
	}
	if len(chunks) == 0 && strings.TrimSpace(body) != "" {
		var parent *int
		if options.ParentChildEnabled {
			value := 0
			parent = &value
			parents = append(parents, MarkdownParent{Index: 0, Heading: "Documento", Content: strings.TrimSpace(body)})
		}
		chunks = append(chunks, MarkdownChunk{Index: 0, Heading: "Documento", Content: strings.TrimSpace(body), ParentIndex: parent, HeadingPath: []string{"Documento"}})
	}
	tags, callouts, anchors := CollectTags(metadata, body), ExtractCallouts(body), ExtractBlockAnchors(body)
	vigencia := ComputeVigencia(metadata, HasDeprecationCallout(callouts), time.Now())
	enriched := BuildEnrichment(metadata, tags, vigencia, anchors, callouts)
	outline := ExtractOutline(body)
	enriched["_outline"] = outline
	chunkPaths, parentPaths := map[string]any{}, map[string]any{}
	for _, chunk := range chunks {
		chunkPaths[fmt.Sprint(chunk.Index)] = chunk.HeadingPath
	}
	for _, parent := range parents {
		parentPaths[fmt.Sprint(parent.Index)] = parent.HeadingPath
	}
	enriched["_chunk_heading_paths"] = chunkPaths
	enriched["_parent_heading_paths"] = parentPaths
	sha, err := CalculateSHA256(path)
	if err != nil {
		return MarkdownDocument{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return MarkdownDocument{}, err
	}
	return MarkdownDocument{Path: path, Title: title, Metadata: enriched, Links: links, Chunks: chunks, SHA256: sha, MTime: float64(info.ModTime().UnixNano()) / 1e9, Parents: parents, Tags: tags, Supplement: SearchableSupplement(tags, callouts), Outline: outline}, nil
}
