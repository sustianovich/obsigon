"use strict";

export function createTraceRenderer({formatContextPercent, openTurnInspector}) {
    function agentTraceTooltip(roleMetrics) {
        const parts = [];
        if (roleMetrics.model) {
            parts.push(`Modelo ${roleMetrics.model}`);
        }
        if (roleMetrics.effective_context_window) {
            parts.push(
                `${roleMetrics.total_tokens ?? "?"}/` +
                `${roleMetrics.effective_context_window} tokens`
            );
        }
        parts.push(roleMetrics.estimated ? "estimado" : "medido");
        if (roleMetrics.incomplete_context_data) {
            parts.push("dato histórico incompleto");
        }
        return parts.join(" · ");
    }

    function createAgentTrace(turn) {
        const agentMetrics = turn?.agent_metrics || turn?.agents;
        if (!agentMetrics || typeof agentMetrics !== "object") {
            return null;
        }
        const trace = document.createElement("div");
        trace.className = "agent-trace";
        const verifier = agentMetrics.verifier || {};
        const writer = agentMetrics.writer || {};
        const verifierItem = document.createElement("span");
        if (verifier.status === "completed" || verifier.status === "degraded") {
            verifierItem.textContent =
                `Verificador ${formatContextPercent(verifier.usage_percent)}`;
            verifierItem.title = agentTraceTooltip(verifier);
        } else if (verifier.status === "error") {
            verifierItem.textContent = "Verificador no disponible";
            verifierItem.classList.add("agent-error");
        } else if (verifier.status === "disabled") {
            verifierItem.textContent = "Verificación desactivada";
        } else {
            verifierItem.textContent = "Sin verificación";
        }
        trace.append(verifierItem);
        if (writer.status === "completed" || writer.status === "degraded") {
            const writerItem = document.createElement("span");
            writerItem.textContent =
                `Redactor ${formatContextPercent(writer.usage_percent)}`;
            writerItem.title = agentTraceTooltip(writer);
            trace.append(writerItem);
        }
        const detailsButton = document.createElement("button");
        detailsButton.type = "button";
        detailsButton.className = "agent-trace-details";
        detailsButton.textContent = "Contexto";
        detailsButton.title =
            "Ver el modelo y la ventana de contexto usados en este turno";
        detailsButton.addEventListener("click", () => {
            openTurnInspector(turn.chat_model, agentMetrics);
        });
        trace.append(detailsButton);
        return trace;
    }

    return {createAgentTrace, createRetrievalTrace};
}

export function createRetrievalTrace(turn) {
    const agentMetrics = turn?.agent_metrics || turn?.agents || {};
    const trace = turn?.retrieval?.trace || agentMetrics.retrieval_trace;
    if (!trace || typeof trace !== "object") {
        return null;
    }
    const details = document.createElement("details");
    details.className = "retrieval-trace";
    const summary = document.createElement("summary");
    summary.textContent = trace.abstained
        ? "Traza de recuperación · abstención"
        : "Traza de recuperación";
    const body = document.createElement("div");
    body.className = "retrieval-trace-body";
    if (trace.abstention_reason) {
        const reason = document.createElement("p");
        reason.textContent = `Motivo: ${trace.abstention_reason}`;
        body.append(reason);
    }
    if (trace.routing) {
        const routing = document.createElement("p");
        routing.textContent =
            `Router: ${trace.routing.class} · grafo ` +
            `${trace.routing.graph_enabled ? "activo" : "inactivo"} · ` +
            `${(trace.routing.reasons || []).join(", ")}`;
        body.append(routing);
    }
    const counts = trace.counts || {};
    const countLine = document.createElement("p");
    countLine.textContent =
        `Candidatos: semántica ${counts.semantic ?? 0}, ` +
        `FTS5 ${counts.lexical ?? 0}, grafo ${counts.graph ?? 0}, ` +
        `fusión ${counts.fused ?? 0}, selección ${counts.selected ?? 0}`;
    body.append(countLine);
    for (const candidate of trace.candidates || []) {
        const row = document.createElement("p");
        const score = (value) => value == null ? "—" : Number(value).toFixed(4);
        row.className = "retrieval-trace-row";
        row.textContent =
            `${candidate.path} · sem ${score(candidate.semantic_score)} · ` +
            `FTS ${score(candidate.lexical_score)} · ` +
            `grafo ${score(candidate.graph_score)} · ` +
            `RRF ${score(candidate.fusion_score)} · ` +
            `hub ${score(candidate.hub_boost)} ` +
            `(${candidate.backlink_count ?? 0} backlinks) · ` +
            `recencia ${score(candidate.recency_boost)} · ` +
            `MMR ${score(candidate.mmr_score)} · ` +
            `rerank ${score(candidate.rerank_score)}`;
        body.append(row);
    }
    details.append(summary, body);
    return details;
}
