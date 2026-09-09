package rag

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

type Block struct {
	Kind   string
	Lines  []string
	Fence  string
	Info   string
	Indent string
	Header []string
}

func (b Block) Text() string { return strings.Trim(strings.Join(b.Lines, "\n"), "\n") }

var (
	tableDelimiterRE = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	listItemRE       = regexp.MustCompile(`^\s{0,3}([-*+]|[0-9]{1,9}[.)])\s+`)
	quoteRE          = regexp.MustCompile(`^\s{0,3}>`)
)

func fenceParts(line string) (indent, fence, info string, ok bool) {
	spaces := 0
	for spaces < len(line) && spaces < 3 && line[spaces] == ' ' {
		spaces++
	}
	if spaces >= len(line) || (line[spaces] != '`' && line[spaces] != '~') {
		return "", "", "", false
	}
	marker := line[spaces]
	end := spaces
	for end < len(line) && line[end] == marker {
		end++
	}
	if end-spaces < 3 {
		return "", "", "", false
	}
	return line[:spaces], line[spaces:end], line[end:], true
}

func isTableStart(lines []string, index int) bool {
	return index+1 < len(lines) && strings.Contains(lines[index], "|") && tableDelimiterRE.MatchString(lines[index+1])
}

func ParseBlocks(body string) []Block {
	lines := strings.Split(body, "\n")
	blocks := make([]Block, 0)
	for index := 0; index < len(lines); {
		line := lines[index]
		if strings.TrimSpace(line) == "" {
			index++
			continue
		}
		if indent, marker, info, ok := fenceParts(line); ok {
			block := Block{Kind: "code", Lines: []string{line}, Fence: marker, Info: strings.TrimSpace(info), Indent: indent}
			index++
			for index < len(lines) {
				candidate := lines[index]
				block.Lines = append(block.Lines, candidate)
				_, closing, closingInfo, found := fenceParts(candidate)
				index++
				if found && closing[0] == marker[0] && len(closing) >= len(marker) && strings.TrimSpace(closingInfo) == "" {
					break
				}
			}
			blocks = append(blocks, block)
			continue
		}
		if isTableStart(lines, index) {
			block := Block{Kind: "table", Lines: []string{lines[index], lines[index+1]}, Header: []string{lines[index], lines[index+1]}}
			index += 2
			for index < len(lines) && strings.TrimSpace(lines[index]) != "" && strings.Contains(lines[index], "|") {
				block.Lines = append(block.Lines, lines[index])
				index++
			}
			blocks = append(blocks, block)
			continue
		}
		if quoteRE.MatchString(line) {
			block := Block{Kind: "callout"}
			for index < len(lines) && (quoteRE.MatchString(lines[index]) || strings.TrimSpace(lines[index]) != "") {
				if !quoteRE.MatchString(lines[index]) && !strings.HasPrefix(lines[index], "  ") {
					break
				}
				block.Lines = append(block.Lines, lines[index])
				index++
			}
			blocks = append(blocks, block)
			continue
		}
		if listItemRE.MatchString(line) {
			block := Block{Kind: "list"}
			for index < len(lines) && strings.TrimSpace(lines[index]) != "" {
				if !listItemRE.MatchString(lines[index]) && !strings.HasPrefix(lines[index], " ") {
					break
				}
				block.Lines = append(block.Lines, lines[index])
				index++
			}
			blocks = append(blocks, block)
			continue
		}
		block := Block{Kind: "paragraph"}
		for index < len(lines) && strings.TrimSpace(lines[index]) != "" {
			_, _, _, isFence := fenceParts(lines[index])
			if isFence || isTableStart(lines, index) || quoteRE.MatchString(lines[index]) {
				break
			}
			block.Lines = append(block.Lines, lines[index])
			index++
		}
		if block.Text() != "" {
			blocks = append(blocks, block)
		} else {
			index++
		}
	}
	return blocks
}

func splitCodeBlock(block Block, maxChars int) []string {
	inner := append([]string(nil), block.Lines[1:]...)
	if len(inner) > 0 {
		if _, _, _, ok := fenceParts(inner[len(inner)-1]); ok {
			inner = inner[:len(inner)-1]
		}
	}
	opening := block.Indent + block.Fence + block.Info
	closing := block.Indent + strings.Repeat(string(block.Fence[0]), len(block.Fence))
	budget := max(80, maxChars-textLength(opening)-textLength(closing)-2)
	parts, current, size := []string{}, []string{}, 0
	flush := func() {
		if len(current) > 0 {
			parts = append(parts, strings.Join(append(append([]string{opening}, current...), closing), "\n"))
			current, size = nil, 0
		}
	}
	for _, line := range inner {
		addition := textLength(line) + 1
		if len(current) > 0 && size+addition > budget {
			flush()
		}
		if addition > budget && len(current) == 0 {
			lineRunes := []rune(line)
			for start := 0; start < len(lineRunes); start += budget {
				end := min(len(lineRunes), start+budget)
				parts = append(parts, strings.Join([]string{opening, string(lineRunes[start:end]), closing}, "\n"))
			}
			continue
		}
		current = append(current, line)
		size += addition
	}
	flush()
	if len(parts) == 0 {
		return []string{block.Text()}
	}
	return parts
}

func splitTableBlock(block Block, maxChars int) []string {
	header := block.Header
	if len(header) == 0 && len(block.Lines) >= 2 {
		header = block.Lines[:2]
	}
	rows := block.Lines[len(header):]
	headerText := strings.Join(header, "\n")
	budget := max(80, maxChars-textLength(headerText)-1)
	parts, current, size := []string{}, []string{}, 0
	for _, row := range rows {
		addition := textLength(row) + 1
		if len(current) > 0 && size+addition > budget {
			parts = append(parts, strings.Join(append([]string{headerText}, current...), "\n"))
			current, size = nil, 0
		}
		current = append(current, row)
		size += addition
	}
	if len(current) > 0 {
		parts = append(parts, strings.Join(append([]string{headerText}, current...), "\n"))
	}
	if len(parts) == 0 {
		return []string{block.Text()}
	}
	return parts
}

func splitPlain(text string, maxChars int) []string {
	parts, current, size := []string{}, []string{}, 0
	for _, line := range strings.Split(text, "\n") {
		addition := textLength(line) + 1
		if len(current) > 0 && size+addition > maxChars {
			parts = append(parts, strings.TrimSpace(strings.Join(current, "\n")))
			current, size = nil, 0
		}
		if addition > maxChars && len(current) == 0 {
			lineRunes := []rune(line)
			for start := 0; start < len(lineRunes); start += maxChars {
				end := min(len(lineRunes), start+maxChars)
				if piece := strings.TrimSpace(string(lineRunes[start:end])); piece != "" {
					parts = append(parts, piece)
				}
			}
			continue
		}
		current = append(current, line)
		size += addition
	}
	if joined := strings.TrimSpace(strings.Join(current, "\n")); joined != "" {
		parts = append(parts, joined)
	}
	return parts
}

func splitBlock(block Block, maxChars int) []string {
	if textLength(block.Text()) <= maxChars {
		return []string{block.Text()}
	}
	switch block.Kind {
	case "code":
		return splitCodeBlock(block, maxChars)
	case "table":
		return splitTableBlock(block, maxChars)
	default:
		return splitPlain(block.Text(), maxChars)
	}
}

func overlapTail(text string, overlapChars int) string {
	if overlapChars <= 0 || text == "" {
		return ""
	}
	lines, tail, size := strings.Split(text, "\n"), []string{}, 0
	for index := len(lines) - 1; index >= 0; index-- {
		addition := textLength(lines[index]) + 1
		if size+addition > overlapChars {
			break
		}
		tail = append([]string{lines[index]}, tail...)
		size += addition
	}
	candidate := strings.TrimSpace(strings.Join(tail, "\n"))
	if strings.Count(candidate, "```")%2 != 0 || strings.Count(candidate, "~~~")%2 != 0 {
		return ""
	}
	return candidate
}

func SplitStructuredText(text string, maxChars, overlapChars int) []string {
	clean := strings.TrimSpace(text)
	if clean == "" {
		return nil
	}
	if textLength(clean) <= maxChars {
		return []string{clean}
	}
	blocks := ParseBlocks(clean)
	if len(blocks) == 0 {
		return splitPlain(clean, maxChars)
	}
	pieces := []string{}
	for _, block := range blocks {
		pieces = append(pieces, splitBlock(block, maxChars)...)
	}
	chunks, current, size := []string{}, []string{}, 0
	for _, piece := range pieces {
		addition := textLength(piece) + 2
		if len(current) > 0 && size+addition > maxChars {
			joined := strings.TrimSpace(strings.Join(current, "\n\n"))
			chunks = append(chunks, joined)
			tail := overlapTail(joined, overlapChars)
			current, size = nil, 0
			if tail != "" {
				current, size = []string{tail}, textLength(tail)+2
			}
		}
		current = append(current, piece)
		size += addition
	}
	if joined := strings.TrimSpace(strings.Join(current, "\n\n")); joined != "" && (len(chunks) == 0 || joined != chunks[len(chunks)-1]) {
		chunks = append(chunks, joined)
	}
	return chunks
}

func HasUnbalancedFence(chunk string) bool {
	return strings.Count(chunk, "```")%2 != 0 || strings.Count(chunk, "~~~")%2 != 0
}

func textLength(value string) int { return utf8.RuneCountInString(value) }
