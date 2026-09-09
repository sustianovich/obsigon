package rag

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestObsidianSyntax(t *testing.T) {
	metadata := map[string]any{"tags": []any{"Protocolo", "SICC-2025"}}
	if got, want := CollectTags(metadata, "Texto con #cribado y #trazabilidad."), []string{"protocolo", "sicc-2025", "cribado", "trazabilidad"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags=%v, want=%v", got, want)
	}
	body := "Texto con #real.\n```bash\n# comentario\n```\nFinal #tambien."
	if got := ExtractInlineTags(body); !reflect.DeepEqual(got, []string{"real", "tambien"}) {
		t.Fatalf("tags en código: %v", got)
	}
	if NormalizeTag("Validación") != "validacion" {
		t.Fatalf("normalización: %s", NormalizeTag("Validación"))
	}
	callouts := ExtractCallouts("> [!caution] Documento derogado\n> Sustituido por v3.")
	if len(callouts) != 1 || callouts[0].Type != "caution" || !HasDeprecationCallout(callouts) {
		t.Fatalf("callout=%v", callouts)
	}
	anchors := ExtractBlockAnchors("El intervalo es de 24 meses. ^poblacion-diana")
	if anchors["poblacion-diana"] != "El intervalo es de 24 meses." {
		t.Fatalf("ancla=%v", anchors)
	}
}

func TestWikiLinksIgnoreCodeAndKeepDetails(t *testing.T) {
	body := "Usa `[[Ejemplo]]`.\n```md\n[[Falso]]\n```\n[[carpeta/Destino#Sección útil|ver]] ![[Roto|vista]]"
	want := []WikiLink{{Target: "carpeta/Destino", Section: "Sección útil", Alias: "ver"}, {Target: "Roto", Alias: "vista", Embedded: true}}
	if got := ExtractWikiLinks(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("links=%#v, want=%#v", got, want)
	}
	got := ExtractWikiLinks(`| Área | [[Hospitalizacion\|hospitalización]] |`)
	if len(got) != 1 || got[0].Target != "Hospitalizacion" || got[0].Alias != "hospitalización" {
		t.Fatalf("alias escapado=%#v", got)
	}
}

func TestVigencia(t *testing.T) {
	today := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	if got := ComputeVigencia(map[string]any{"fecha_vigencia": "2025-01-01"}, false, today); got.TemporalStatus != "vigente" {
		t.Fatalf("vigencia=%v", got)
	}
	if got := ComputeVigencia(map[string]any{"fecha_vigencia": "2027-01-01"}, true, today); got.TemporalStatus != "futura" {
		t.Fatalf("futura=%v", got)
	}
	if got := ComputeVigencia(map[string]any{"fecha_vigencia": "2020-01-01"}, true, today); got.TemporalStatus != "caducada" {
		t.Fatalf("caducada=%v", got)
	}
}

func TestParseMarkdownParentChild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nota.md")
	content := "# Tema\n\n"
	for i := 0; i < 120; i++ {
		content += "termino-largo "
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := ParseMarkdownFile(path, ParseOptions{ChunkSize: 1800, ChunkOverlap: 250, ParentChildEnabled: true, ParentChunkSize: 220, ChildChunkSize: 90, ChildChunkOverlap: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Parents) < 2 || len(doc.Chunks) <= len(doc.Parents) {
		t.Fatalf("padres=%d hijos=%d", len(doc.Parents), len(doc.Chunks))
	}
	parents := map[int]string{}
	for _, parent := range doc.Parents {
		parents[parent.Index] = parent.Content
	}
	for _, child := range doc.Chunks {
		if child.ParentIndex == nil || !strings.Contains(parents[*child.ParentIndex], child.Content) {
			t.Fatalf("hijo sin padre: %#v", child)
		}
	}
}

func TestMarkdownHierarchyAndOutlineArePreserved(t *testing.T) {
	body := "# Nota\n\nIntroducción.\n\n## Decisión\n\nSe adopta X.\n\n### Motivo\n\nSe relaciona con Y.\n\n## Cierre\n\nFin."
	sections := SplitSectionsWithHierarchy(body)
	want := map[string][]string{
		"Nota":     {"Nota"},
		"Decisión": {"Nota", "Decisión"},
		"Motivo":   {"Nota", "Decisión", "Motivo"},
		"Cierre":   {"Nota", "Cierre"},
	}
	for _, section := range sections {
		if expected, ok := want[section.Heading]; ok && !reflect.DeepEqual(section.Path, expected) {
			t.Fatalf("path de %s=%v, want=%v", section.Heading, section.Path, expected)
		}
	}
	outline := ExtractOutline(body)
	if len(outline) != 4 || !reflect.DeepEqual(outline[2].Path, want["Motivo"]) {
		t.Fatalf("outline=%+v", outline)
	}

	path := filepath.Join(t.TempDir(), "nota.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := ParseMarkdownFile(path, ParseOptions{ChunkSize: 1800, ChunkOverlap: 250})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Outline) != 4 || len(doc.Chunks) != 4 {
		t.Fatalf("outline=%d chunks=%d", len(doc.Outline), len(doc.Chunks))
	}
	paths, ok := doc.Metadata["_chunk_heading_paths"].(map[string]any)
	if !ok || !reflect.DeepEqual(paths["2"], want["Motivo"]) {
		t.Fatalf("paths derivados=%#v", doc.Metadata["_chunk_heading_paths"])
	}
}
