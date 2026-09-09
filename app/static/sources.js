"use strict";

export function documentStatus(metadata) {
    const raw = String(metadata?.estado ?? metadata?.status ?? "")
        .trim()
        .toLocaleLowerCase("es");
    const known = {
        vigente: {label: "Vigente", className: "vigente"},
        borrador: {label: "Borrador", className: "borrador"},
        derogado: {label: "Derogado", className: "derogado"},
    };
    return known[raw] ?? {
        label: raw || "Sin estado",
        className: "sin-estado",
    };
}

export function safeDomId(value) {
    return String(value).replace(/[^a-zA-Z0-9_-]/g, "-");
}

export function focusSource(turnKey, referenceNumber) {
    const card = document.getElementById(
        `source-${safeDomId(turnKey)}-${referenceNumber}`
    );
    if (!card) {
        return;
    }
    const sourceGroup = card.closest(".turn-sources");
    if (sourceGroup) {
        sourceGroup.open = true;
    }
    card.open = true;
    card.classList.remove("source-highlight");
    void card.offsetWidth;
    card.classList.add("source-highlight");
    card.scrollIntoView({behavior: "smooth", block: "center"});
}

export function renderAnswer(container, answer, sourceCount, turnKey) {
    container.replaceChildren();
    const citationPattern = /\[(\d+)\]/g;
    let cursor = 0;
    for (const match of answer.matchAll(citationPattern)) {
        container.append(document.createTextNode(answer.slice(cursor, match.index)));
        const referenceNumber = Number(match[1]);
        if (referenceNumber >= 1 && referenceNumber <= sourceCount) {
            const citation = document.createElement("button");
            citation.type = "button";
            citation.className = "citation";
            citation.textContent = match[0];
            citation.title = `Ver fuente ${referenceNumber}`;
            citation.setAttribute("aria-label", `Ver fuente ${referenceNumber}`);
            citation.addEventListener(
                "click", () => focusSource(turnKey, referenceNumber)
            );
            container.append(citation);
        } else {
            container.append(document.createTextNode(match[0]));
        }
        cursor = match.index + match[0].length;
    }
    container.append(document.createTextNode(answer.slice(cursor)));
}

export async function copyTextToClipboard(text) {
    if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text);
        return;
    }
    const copyArea = document.createElement("textarea");
    copyArea.value = text;
    copyArea.setAttribute("readonly", "");
    copyArea.className = "clipboard-fallback";
    document.body.append(copyArea);
    copyArea.select();
    const copied = document.execCommand("copy");
    copyArea.remove();
    if (!copied) {
        throw new Error("No se pudo copiar la respuesta.");
    }
}

export function createCopyAnswerButton(getText) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "copy-answer-button";
    button.textContent = "Copiar";
    button.title = "Copiar respuesta";
    button.setAttribute("aria-label", "Copiar respuesta al portapapeles");
    button.addEventListener("click", async () => {
        const text = getText().trim();
        if (!text) {
            return;
        }
        button.disabled = true;
        try {
            await copyTextToClipboard(text);
            button.textContent = "Copiado";
        } catch (error) {
            button.textContent = "No copiado";
            console.error(error);
        } finally {
            window.setTimeout(() => {
                button.disabled = false;
                button.textContent = "Copiar";
            }, 1600);
        }
    });
    return button;
}

export function createAssistantHeader(label, actions = []) {
    const header = document.createElement("div");
    header.className = "assistant-header";
    header.append(label, ...actions);
    return header;
}

const VIGENCIA_LABELS = {
    caducada: {label: "Derogado", className: "vigencia-caducada"},
    futura: {label: "Aún no vigente", className: "vigencia-futura"},
};

export function vigenciaBadge(vigencia) {
    if (!vigencia?.estado_temporal) {
        return null;
    }
    const config = VIGENCIA_LABELS[vigencia.estado_temporal];
    if (!config) {
        return null;
    }
    const badge = document.createElement("span");
    badge.className = `vigencia-badge ${config.className}`;
    badge.textContent = config.label;
    const detail = [];
    if (vigencia.valid_from) {
        detail.push(`en vigor desde ${vigencia.valid_from}`);
    }
    if (vigencia.valid_until) {
        detail.push(`derogado el ${vigencia.valid_until}`);
    }
    if (vigencia.derogado_por_callout) {
        detail.push("el propio documento se declara derogado");
    }
    badge.title = detail.join(" · ") || config.label;
    return badge;
}

export function createSourceCard(source, index, turnKey) {
    const card = document.createElement("details");
    card.className = "source-card";
    card.id = `source-${safeDomId(turnKey)}-${index}`;

    const summary = document.createElement("summary");
    summary.className = "source-summary";
    const reference = document.createElement("div");
    reference.className = "source-reference";
    reference.textContent = source.reference;
    const overview = document.createElement("div");
    overview.className = "source-overview";
    const titleRow = document.createElement("div");
    titleRow.className = "source-title-row";
    const title = document.createElement("h4");
    title.className = "source-title";
    title.textContent = source.title;
    const statusData = documentStatus(source.metadata);
    const status = document.createElement("span");
    status.className = `document-status ${statusData.className}`;
    status.textContent = statusData.label;
    titleRow.append(title, status);
    const validity = vigenciaBadge(source.vigencia);
    if (validity) {
        titleRow.append(validity);
    }
    for (const tag of (source.tags || []).slice(0, 4)) {
        const chip = document.createElement("span");
        chip.className = "source-tag";
        chip.textContent = `#${tag}`;
        titleRow.append(chip);
    }
    const detail = document.createElement("p");
    detail.className = "source-detail";
    detail.textContent = `${source.path} · ${source.heading} · ${source.reason}`;
    overview.append(titleRow, detail);

    const score = document.createElement("div");
    score.className = "source-score";
    score.textContent = Number(source.score).toFixed(4);
    const scoreParts = ["Puntuación de recuperación"];
    if (source.semantic_score != null) {
        scoreParts.push(
            `similitud semántica ${Number(source.semantic_score).toFixed(4)}`
        );
    }
    if (source.lexical_score != null) {
        scoreParts.push(`BM25 textual ${Number(source.lexical_score).toFixed(4)}`);
    }
    score.title = scoreParts.join(" · ");
    const chevron = document.createElement("span");
    chevron.className = "source-chevron";
    chevron.setAttribute("aria-hidden", "true");
    chevron.textContent = "›";
    summary.append(reference, overview, score, chevron);

    const fragment = document.createElement("div");
    fragment.className = "source-fragment";
    const fragmentLabel = document.createElement("span");
    fragmentLabel.className = "source-fragment-label";
    fragmentLabel.textContent = source.context_expanded
        ? "Contexto padre"
        : "Fragmento recuperado";
    const fragmentText = document.createElement("p");
    fragmentText.textContent = source.content;
    fragment.append(fragmentLabel, fragmentText);
    if (source.obsidian_url) {
        const openInObsidian = document.createElement("a");
        openInObsidian.className = "obsidian-link";
        openInObsidian.href = source.obsidian_url;
        openInObsidian.textContent = "Abrir en Obsidian";
        fragment.append(openInObsidian);
    }
    if (source.context_expanded
        && source.matched_content
        && source.matched_content !== source.content) {
        const matchLabel = document.createElement("span");
        matchLabel.className = "source-fragment-label";
        const matchCount = Array.isArray(source.matched_chunk_ids)
            ? source.matched_chunk_ids.length
            : 1;
        matchLabel.textContent = matchCount > 1
            ? `Coincidencia encontrada (mejor de ${matchCount} fragmentos de esta sección)`
            : "Coincidencia encontrada";
        const matchText = document.createElement("p");
        matchText.textContent = source.matched_content;
        fragment.append(matchLabel, matchText);
    }
    card.append(summary, fragment);
    return card;
}
