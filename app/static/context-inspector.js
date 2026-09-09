"use strict";

import {request as apiRequest} from "./api.js";
import {state} from "./state.js";

export function formatContextPercent(value) {
    const number = Number(value);
    return Number.isFinite(number) ? `${number.toFixed(1)}%` : "—";
}

export function contextLevel(percent) {
    if (!Number.isFinite(percent)) {
        return "empty";
    }
    if (percent > 85) {
        return "danger";
    }
    return percent >= 60 ? "warning" : "safe";
}

export function formatContextWindow(value) {
    const number = Number(value);
    if (!Number.isFinite(number) || number <= 0) {
        return "—";
    }
    if (number >= 1024) {
        const kilo = number / 1024;
        return Number.isInteger(kilo) ? `${kilo}K` : `${kilo.toFixed(1)}K`;
    }
    return String(number);
}

export function maxUsagePercentFromAgents(agents) {
    if (!agents || typeof agents !== "object") {
        return null;
    }
    const percentages = [];
    for (const role of ["verifier", "writer"]) {
        const metrics = agents[role];
        if (metrics?.status === "completed" || metrics?.status === "degraded") {
            const value = Number(metrics.usage_percent);
            if (Number.isFinite(value)) {
                percentages.push(value);
            }
        }
    }
    return percentages.length ? Math.max(...percentages) : null;
}

const AGENT_STATUS_LABELS = {
    completed: "Activado",
    degraded: "Activado (degradado)",
    disabled: "Desactivado",
    pending: "Pendiente",
    skipped: "Omitido",
    skipped_no_evidence: "Omitido: sin evidencia recuperada",
    skipped_insufficient_evidence: "Omitido: evidencia insuficiente",
    error: "Error",
};

export function createContextInspector({elements, supportsDialog}) {
    function refreshContextIndicatorHeader() {
        if (!elements.contextIndicator) {
            return;
        }
        const modelContext = state.lastStatusData?.model_context;
        if (!modelContext) {
            elements.contextIndicatorPercent.textContent = "—";
            elements.contextIndicatorModel.textContent = "Cargando…";
            elements.contextIndicatorWindow.textContent = "—/—";
            return;
        }
        const project = state.loadedProjects.find(
            (candidate) => candidate.id === state.activeProjectId
        );
        const configuredWriter =
            Number(project?.agent_settings?.writer_context_tokens) || 16384;
        const effective = Math.min(
            configuredWriter, modelContext.model_context_window
        );
        const percent = state.latestTurnMaxUsagePercent
            ?? project?.context_usage?.latest_percent;
        elements.contextIndicatorPercent.textContent =
            percent == null ? "—" : formatContextPercent(percent);
        elements.contextIndicatorModel.textContent = modelContext.model;
        elements.contextIndicatorWindow.textContent =
            `${formatContextWindow(effective)}/` +
            `${formatContextWindow(modelContext.model_context_window)}`;
        elements.contextIndicator.dataset.level = contextLevel(Number(percent));
        elements.contextIndicator.title = modelContext.verified
            ? `Fuente de la capacidad: ${modelContext.source}. Pulsa para ` +
              "abrir el inspector de contexto."
            : `Capacidad sin verificar (${modelContext.source}); se usa un ` +
              "límite prudente. Pulsa para abrir el inspector de contexto.";
    }

    function renderContextRolePanel(container, metrics) {
        if (!container) {
            return;
        }
        container.replaceChildren();
        if (!metrics || typeof metrics !== "object" || !metrics.status) {
            const empty = document.createElement("p");
            empty.className = "context-inspector-empty";
            empty.textContent = "Todavía no hay datos para este agente.";
            container.append(empty);
            return;
        }
        const rows = [
            ["Estado", AGENT_STATUS_LABELS[metrics.status] || metrics.status],
            ["Modelo usado", metrics.model || "—"],
            ["Ventana configurada", formatContextWindow(
                metrics.configured_context_window
            )],
            ["Ventana efectiva", formatContextWindow(
                metrics.effective_context_window
            )],
        ];
        const measuring =
            metrics.status === "completed" || metrics.status === "degraded";
        if (measuring) {
            rows.push(
                ["Entrada", `${metrics.prompt_tokens ?? "—"} tokens`],
                ["Salida", `${metrics.completion_tokens ?? "—"} tokens`],
                ["Total", `${metrics.total_tokens ?? "—"} tokens`],
                ["Margen disponible", `${metrics.remaining_tokens ?? "—"} tokens`]
            );
        }
        const dl = document.createElement("dl");
        dl.className = "context-inspector-grid";
        for (const [label, value] of rows) {
            const row = document.createElement("div");
            const dt = document.createElement("dt");
            const dd = document.createElement("dd");
            dt.textContent = label;
            dd.textContent = value;
            row.append(dt, dd);
            dl.append(row);
        }
        container.append(dl);
        if (measuring) {
            const percent = Number(metrics.usage_percent);
            const meterWrap = document.createElement("div");
            meterWrap.className = "context-meter";
            meterWrap.setAttribute("role", "img");
            meterWrap.setAttribute(
                "aria-label", `Uso de contexto: ${formatContextPercent(percent)}`
            );
            const meterFill = document.createElement("span");
            meterFill.dataset.level = contextLevel(percent);
            meterFill.style.width = Number.isFinite(percent)
                ? `${Math.min(100, Math.max(0, percent))}%`
                : "0%";
            meterWrap.append(meterFill);
            container.append(meterWrap);
            const note = document.createElement("p");
            note.className = "context-inspector-note";
            note.textContent = metrics.estimated
                ? "Estimado: Ollama no informó contadores exactos en esta inferencia."
                : "Medido: contadores reales informados por Ollama.";
            container.append(note);
        }
        if (metrics.context_warning) {
            const warning = document.createElement("p");
            warning.className = "context-inspector-warning";
            warning.textContent = metrics.context_warning;
            container.append(warning);
        }
        if (metrics.incomplete_context_data) {
            const incomplete = document.createElement("p");
            incomplete.className = "context-inspector-warning";
            incomplete.textContent =
                "Dato histórico incompleto: este turno se guardó antes de " +
                "registrar todos estos campos.";
            container.append(incomplete);
        }
    }

    function renderContextInspectorModel(modelContext) {
        if (!modelContext) {
            for (const element of [
                elements.ciModelName,
                elements.ciModelProfile,
                elements.ciModelCapacity,
                elements.ciModelSource,
                elements.ciModelVerified,
            ]) {
                element.textContent = "—";
            }
            elements.ciModelWarning.hidden = true;
            return;
        }
        elements.ciModelName.textContent = modelContext.model || "—";
        elements.ciModelProfile.textContent = modelContext.profile_id || "Ninguno";
        elements.ciModelCapacity.textContent = formatContextWindow(
            modelContext.model_context_window
        );
        const sourceLabels = {
            profile: "Perfil conocido",
            ollama: "Metadatos de Ollama",
            "profile+ollama": "Perfil + Ollama",
            fallback: "Límite prudente (sin datos)",
        };
        elements.ciModelSource.textContent =
            sourceLabels[modelContext.source] || modelContext.source || "—";
        elements.ciModelVerified.textContent = modelContext.verified ? "Sí" : "No";
        if (modelContext.warning) {
            elements.ciModelWarning.textContent = modelContext.warning;
            elements.ciModelWarning.hidden = false;
        } else {
            elements.ciModelWarning.hidden = true;
        }
    }

    function renderContextInspectorConversation(overview) {
        renderContextInspectorModel(overview.current_model);
        elements.ciConversationEmpty.hidden = true;
        elements.ciConversationGrid.hidden = false;
        const history = overview.history || {};
        elements.ciTotalTurns.textContent = String(history.total_turns ?? 0);
        elements.ciStoredTokens.textContent =
            `~${history.stored_estimated_tokens ?? 0} tokens (estimado)`;
        elements.ciRecentTurns.textContent = String(
            history.recent_turns_included ?? 0
        );
        const recentParts = [`~${history.recent_estimated_tokens ?? 0} tokens`];
        if (history.summary_included) {
            recentParts.push("+ resumen");
        }
        elements.ciRecentTokens.textContent =
            `${recentParts.join(" ")} (estimado)`;
        elements.ciSummaryTokens.textContent = history.summary_included
            ? `~${history.summary_estimated_tokens ?? 0} tokens (estimado)`
            : "Sin resumen todavía";
        elements.ciProjectMemoryTokens.textContent =
            history.project_memory_estimated_tokens
                ? `~${history.project_memory_estimated_tokens} tokens (estimado)`
                : "Sin memoria de proyecto";
        const nextEstimate = overview.next_inference_estimate || {};
        elements.ciNextEstimate.textContent =
            `~${nextEstimate.estimated_prompt_tokens ?? "—"} tokens de ` +
            `${formatContextWindow(nextEstimate.effective_context_window)} ` +
            `(${formatContextPercent(nextEstimate.estimated_usage_percent)}, estimado)`;
        const lastInference = overview.last_inference || {};
        renderContextRolePanel(elements.ciVerifier, lastInference.verifier);
        renderContextRolePanel(elements.ciWriter, lastInference.writer);
        const warnings = Array.isArray(overview.warnings) ? overview.warnings : [];
        if (warnings.length) {
            elements.ciWarnings.textContent = warnings.join(" ");
            elements.ciWarnings.hidden = false;
        } else {
            elements.ciWarnings.hidden = true;
        }
    }

    async function refreshContextInspector() {
        renderContextInspectorModel(state.lastStatusData?.model_context);
        if (!state.activeConversationId) {
            elements.ciConversationEmpty.hidden = false;
            elements.ciConversationGrid.hidden = true;
            renderContextRolePanel(elements.ciVerifier, null);
            renderContextRolePanel(elements.ciWriter, null);
            elements.ciWarnings.hidden = true;
            return;
        }
        try {
            const overview = await apiRequest(
                `/api/conversations/${encodeURIComponent(
                    state.activeConversationId
                )}/context`
            );
            renderContextInspectorConversation(overview);
        } catch (error) {
            elements.ciConversationEmpty.hidden = false;
            elements.ciConversationEmpty.textContent = error.message;
            elements.ciConversationGrid.hidden = true;
        }
    }

    function openContextInspector() {
        if (!supportsDialog(elements.contextInspectorDialog)) {
            elements.systemMessage.textContent =
                "El inspector de contexto requiere un navegador con soporte " +
                "para <dialog>.";
            return;
        }
        elements.contextInspectorDialog.showModal();
        refreshContextInspector();
    }

    function openTurnInspector(chatModel, agentMetrics) {
        if (!supportsDialog(elements.turnInspectorDialog)) {
            elements.queryStatus.textContent =
                "El detalle de contexto requiere un navegador con soporte " +
                "para <dialog>.";
            return;
        }
        elements.turnInspectorModel.textContent = chatModel
            ? `Modelo utilizado en este turno: ${chatModel}`
            : "Modelo utilizado en este turno: dato histórico incompleto.";
        renderContextRolePanel(
            elements.turnInspectorVerifier, agentMetrics?.verifier
        );
        renderContextRolePanel(
            elements.turnInspectorWriter, agentMetrics?.writer
        );
        elements.turnInspectorDialog.showModal();
    }

    return {
        openContextInspector,
        openTurnInspector,
        refreshContextIndicatorHeader,
        refreshContextInspector,
        renderContextInspectorConversation,
        renderContextInspectorModel,
        renderContextRolePanel,
    };
}
