package rag

import (
	"context"
	"strings"
)

// expandSelectedSections keeps ranking at chunk level but replaces the prompt
// evidence with the complete Markdown section after selection. Failures are
// deliberately soft: a stale/moved file keeps its indexed parent/chunk.
func (r *Retriever) expandSelectedSections(ctx context.Context, hits []map[string]any) {
	if !r.Settings.ReadLargeEnabled || r.Store == nil || len(hits) == 0 {
		return
	}
	toolbox := AgentToolbox{Store: r.Store, VaultPath: r.Settings.VaultPath}
	for _, hit := range hits {
		path, heading := stringValue(hit["path"]), stringValue(hit["heading"])
		_, body, err := toolbox.readIndexedBody(ctx, path)
		if err != nil {
			continue
		}
		actualHeading, section, ok := extractLargeMarkdownSection(body, heading)
		if !ok || strings.TrimSpace(section) == "" {
			continue
		}
		matched := stringValue(hit["matched_content"])
		if matched == "" {
			matched = stringValue(hit["content"])
		}
		hit["matched_content"] = matched
		hit["content"] = section
		hit["heading"] = actualHeading
		hit["heading_path"] = markdownHeadingPath(body, actualHeading)
		hit["outline"] = markdownOutline(body)
		hit["context_expanded"] = true
		hit["context_expansion"] = "full_section"
		if reason := strings.TrimSpace(stringValue(hit["reason"])); !strings.Contains(reason, "sección completa") {
			if reason == "" {
				hit["reason"] = "sección completa"
			} else {
				hit["reason"] = reason + " + sección completa"
			}
		}
	}
}

func extractLargeMarkdownSection(body, heading string) (string, string, bool) {
	wanted := normalizeLexical(strings.TrimSpace(heading))
	switch wanted {
	case "documento":
		if len(markdownOutlineRE.FindAllStringIndex(body, -1)) == 0 {
			return "Documento", strings.TrimSpace(body), strings.TrimSpace(body) != ""
		}
	case "introduccion":
		if first := markdownOutlineRE.FindStringIndex(body); first != nil {
			intro := strings.TrimSpace(body[:first[0]])
			return "Introducción", intro, intro != ""
		}
	}
	return extractMarkdownSection(body, heading)
}

func markdownHeadingPath(body, wanted string) []string {
	indices := markdownOutlineRE.FindAllStringSubmatchIndex(body, -1)
	stack := []string{}
	wanted = normalizeLexical(strings.TrimSpace(strings.TrimLeft(wanted, "#")))
	for _, match := range indices {
		level := match[3] - match[2]
		heading := strings.TrimSpace(body[match[4]:match[5]])
		if len(stack) >= level {
			stack = stack[:level-1]
		}
		stack = append(stack, heading)
		if normalizeLexical(heading) == wanted {
			return append([]string(nil), stack...)
		}
	}
	if wanted == "introduccion" {
		return []string{"Introducción"}
	}
	return nil
}
