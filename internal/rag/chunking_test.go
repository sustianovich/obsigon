package rag

import (
	"fmt"
	"strings"
	"testing"
)

func TestStructuredChunkingPreservesCodeFences(t *testing.T) {
	code := "```python\n" + strings.Repeat("linea = 1\n", 60) + "```"
	for _, limit := range []int{150, 300, 600, 1200} {
		chunks := SplitStructuredText("Introducción.\n\n"+code+"\n\nCierre.", limit, 40)
		for _, chunk := range chunks {
			if HasUnbalancedFence(chunk) {
				t.Fatalf("valla rota con límite %d: %q", limit, chunk)
			}
		}
	}
	chunks := SplitStructuredText(code, 200, 0)
	if len(chunks) < 2 {
		t.Fatal("el bloque largo no se dividió")
	}
	for _, chunk := range chunks {
		if !strings.HasPrefix(chunk, "```python") || !strings.HasSuffix(strings.TrimSpace(chunk), "```") {
			t.Fatalf("bloque de código inválido: %q", chunk)
		}
	}
}

func TestStructuredChunkingRepeatsTableHeader(t *testing.T) {
	table := "| Codigo | Indicador | Objetivo |\n| --- | --- | --- |\n"
	for n := 0; n < 30; n++ {
		table += fmt.Sprintf("| IP-%02d | Indicador %d | %d %% |\n", n, n, n)
	}
	chunks := SplitStructuredText(table, 250, 0)
	if len(chunks) < 2 {
		t.Fatal("la tabla larga no se dividió")
	}
	for _, chunk := range chunks {
		if !strings.Contains(chunk, "| Codigo | Indicador | Objetivo |") || !strings.Contains(chunk, "| --- |") {
			t.Fatalf("cabecera ausente: %q", chunk)
		}
	}
}

func TestParseBlockKinds(t *testing.T) {
	body := "Párrafo.\n\n```go\nx := 1\n```\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\n> [!note] Aviso\n> Cuerpo"
	kinds := map[string]bool{}
	for _, block := range ParseBlocks(body) {
		kinds[block.Kind] = true
	}
	for _, kind := range []string{"paragraph", "code", "table", "callout"} {
		if !kinds[kind] {
			t.Fatalf("no se detectó %s", kind)
		}
	}
}

func TestChunkLimitsCountUnicodeCharactersNotBytes(t *testing.T) {
	chunks := SplitStructuredText(strings.Repeat("á", 250), 100, 0)
	if len(chunks) != 3 {
		t.Fatalf("fragmentos=%d, se esperaban 3", len(chunks))
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) > 100 || strings.ToValidUTF8(chunk, "") != chunk {
			t.Fatalf("fragmento Unicode inválido: %q", chunk)
		}
	}
}
