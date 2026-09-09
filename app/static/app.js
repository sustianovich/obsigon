import {request as apiRequest} from "./api.js";
import {createChatController} from "./chat.js";
import {createProjectsController} from "./projects.js";
import {state, STORAGE_KEYS} from "./state.js";
import {createTraceRenderer} from "./retrieval-trace.js";
import {
    contextLevel,
    createContextInspector,
    formatContextPercent,
    formatContextWindow,
    maxUsagePercentFromAgents,
} from "./context-inspector.js";

const elements = {
	appVersion: document.getElementById("app-version"),
    desktop: document.querySelector(".desktop"),
    themeToggle: document.getElementById("theme-toggle"),
    sidebar: document.querySelector(".sidebar"),
    sidebarAdvancedToggle: document.getElementById(
        "sidebar-advanced-toggle"
    ),
    mobileTabs: Array.from(document.querySelectorAll(".mobile-tab")),
    workspace: document.getElementById("workspace"),
    welcome: document.getElementById("welcome"),
    conversationHeader: document.getElementById("conversation-header"),
    conversationTitle: document.getElementById("conversation-title"),
    chatThread: document.getElementById("chat-thread"),
    conversationList: document.getElementById("conversation-list"),
    conversationListEmpty: document.getElementById(
        "conversation-list-empty"
    ),
	conversationLoadMore: document.getElementById("conversation-load-more"),
    projectList: document.getElementById("project-list"),
    projectListEmpty: document.getElementById("project-list-empty"),
    newProjectButton: document.getElementById("new-project-button"),
    deleteProjectButton: document.getElementById("delete-project-button"),
    projectAgentSettings: document.getElementById(
        "project-agent-settings"
    ),
    projectVerificationEnabled: document.getElementById(
        "project-verification-enabled"
    ),
    projectMemoryEnabled: document.getElementById(
        "project-memory-enabled"
    ),
    verifierContextTokens: document.getElementById(
        "verifier-context-tokens"
    ),
    writerContextTokens: document.getElementById(
        "writer-context-tokens"
    ),
    verifierContextValue: document.getElementById(
        "verifier-context-value"
    ),
    writerContextValue: document.getElementById(
        "writer-context-value"
    ),
    verifierContextUsage: document.getElementById(
        "verifier-context-usage"
    ),
    writerContextUsage: document.getElementById(
        "writer-context-usage"
    ),
    verifierContextBar: document.getElementById(
        "verifier-context-bar"
    ),
    writerContextBar: document.getElementById("writer-context-bar"),
    projectContextOverall: document.getElementById(
        "project-context-overall"
    ),
    projectMemorySummaryStatus: document.getElementById(
        "project-memory-summary-status"
    ),
    saveProjectAgentSettings: document.getElementById(
        "save-project-agent-settings"
    ),
    workspaceActiveProject: document.getElementById(
        "workspace-active-project"
    ),
    newConversationButton: document.getElementById(
        "new-conversation-button"
    ),
    indexButton: document.getElementById("index-button"),
    selectVaultButton: document.getElementById("select-vault-button"),
    chatProfile: document.getElementById("chat-profile"),
    customChatProfile: document.getElementById("custom-chat-profile"),
    documentCount: document.getElementById("document-count"),
    chunkCount: document.getElementById("chunk-count"),
    vaultPath: document.getElementById("vault-path"),
    watchStatus: document.getElementById("watch-status"),
    graphHealthHint: document.getElementById("graph-health-hint"),
    systemMessage: document.getElementById("system-message"),
    form: document.getElementById("question-form"),
    question: document.getElementById("question"),
    askButton: document.getElementById("ask-button"),
    stopQueryButton: document.getElementById("stop-query-button"),
    queryStatus: document.getElementById("query-status"),
    topK: document.getElementById("top-k"),
    statusFilter: document.getElementById("status-filter"),
    vigenciaFilter: document.getElementById("vigencia-filter"),
    tagFilter: document.getElementById("tag-filter"),
    tagFilterControl: document.getElementById("tag-filter-control"),
    tagFilterSummary: document.getElementById("tag-filter-summary"),
    tagFilterSearch: document.getElementById("tag-filter-search"),
    tagFilterClear: document.getElementById("tag-filter-clear"),
    tagFilterEmpty: document.getElementById("tag-filter-empty"),
    contextCeilingHint: document.getElementById("context-ceiling-hint"),
    historyDrawer: document.getElementById("history-drawer"),
    expandLinks: document.getElementById("expand-links"),
    expandLinksLabel: document.getElementById("expand-links-label"),
    useMemory: document.getElementById("use-memory"),
    memoryStatus: document.getElementById("memory-status"),
    textDialog: document.getElementById("text-dialog"),
    textDialogTitle: document.getElementById("text-dialog-title"),
    textDialogLabel: document.getElementById("text-dialog-label"),
    textDialogInput: document.getElementById("text-dialog-input"),
    textDialogConfirm: document.getElementById("text-dialog-confirm"),
    confirmDialog: document.getElementById("confirm-dialog"),
    confirmDialogTitle: document.getElementById("confirm-dialog-title"),
    confirmDialogMessage: document.getElementById(
        "confirm-dialog-message"
    ),
    confirmDialogConfirm: document.getElementById(
        "confirm-dialog-confirm"
    ),
    contextIndicator: document.getElementById("context-indicator"),
    contextIndicatorPercent: document.getElementById(
        "context-indicator-percent"
    ),
    contextIndicatorModel: document.getElementById(
        "context-indicator-model"
    ),
    contextIndicatorWindow: document.getElementById(
        "context-indicator-window"
    ),
    contextInspectorDialog: document.getElementById(
        "context-inspector-dialog"
    ),
    ciModelName: document.getElementById("ci-model-name"),
    ciModelProfile: document.getElementById("ci-model-profile"),
    ciModelCapacity: document.getElementById("ci-model-capacity"),
    ciModelSource: document.getElementById("ci-model-source"),
    ciModelVerified: document.getElementById("ci-model-verified"),
    ciModelWarning: document.getElementById("ci-model-warning"),
    ciConversationEmpty: document.getElementById("ci-conversation-empty"),
    ciConversationGrid: document.getElementById("ci-conversation-grid"),
    ciTotalTurns: document.getElementById("ci-total-turns"),
    ciStoredTokens: document.getElementById("ci-stored-tokens"),
    ciRecentTurns: document.getElementById("ci-recent-turns"),
    ciRecentTokens: document.getElementById("ci-recent-tokens"),
    ciSummaryTokens: document.getElementById("ci-summary-tokens"),
    ciProjectMemoryTokens: document.getElementById(
        "ci-project-memory-tokens"
    ),
    ciNextEstimate: document.getElementById("ci-next-estimate"),
    ciVerifier: document.getElementById("ci-verifier"),
    ciWriter: document.getElementById("ci-writer"),
    ciWarnings: document.getElementById("ci-warnings"),
    turnInspectorDialog: document.getElementById("turn-inspector-dialog"),
    turnInspectorModel: document.getElementById("turn-inspector-model"),
    turnInspectorVerifier: document.getElementById(
        "turn-inspector-verifier"
    ),
    turnInspectorWriter: document.getElementById("turn-inspector-writer"),
};

function preferredMemoryEnabled() {
    return window.localStorage.getItem(STORAGE_KEYS.memoryPreference) !== "false";
}

function scrollToLatestTurn() {
    elements.chatThread.scrollTop = elements.chatThread.scrollHeight;
}

function setChatLayout(hasMessages, title = "") {
    elements.workspace.classList.toggle("has-messages", hasMessages);
    elements.welcome.classList.toggle("hidden", hasMessages);
    elements.conversationHeader.classList.toggle("hidden", !hasMessages);
    elements.chatThread.classList.toggle("hidden", !hasMessages);
    elements.conversationTitle.textContent = title || "Nueva conversación";
}

function updateMemoryStatus(memory = {}) {
    if (memory.enabled === false) {
        elements.memoryStatus.textContent = "Memoria desactivada para este hilo.";
        return;
    }
    const totalTurns = Number(memory.total_turns ?? 0);
    const summarizedTurns = Number(memory.summarized_turns ?? 0);
    let message = "Memoria activa.";
    if (memory.has_summary && summarizedTurns > 0) {
        message = `Memoria activa · ${summarizedTurns} turnos resumidos.`;
    } else if (totalTurns > 0) {
        message = `Memoria activa · ${totalTurns} turnos disponibles.`;
    }
    if (memory.last_error || memory.warning) {
        message += ` ${memory.last_error || memory.warning}`;
    }
    elements.memoryStatus.textContent = message;
}

elements.useMemory.checked = preferredMemoryEnabled();

const {
    openContextInspector,
    openTurnInspector,
    refreshContextIndicatorHeader,
    renderContextRolePanel,
} = createContextInspector({elements, supportsDialog});
const {createAgentTrace, createRetrievalTrace} = createTraceRenderer({
    formatContextPercent,
    openTurnInspector,
});
const {
    createTurnElement,
    renderConversation,
    startNewConversation,
    streamQuery,
} = createChatController({
    elements,
    createAgentTrace,
    createRetrievalTrace,
    markActiveConversation,
    preferredMemoryEnabled,
    refreshContextIndicatorHeader,
    scrollToLatestTurn,
    setChatLayout,
    setMobileTab,
    updateMemoryStatus,
});
const {
    closeHistoryDrawer,
    loadConversations,
    loadProjects,
} = createProjectsController({
    askForConfirmation,
    askForText,
    elements,
    markActiveConversation,
    markActiveProject,
    refreshContextIndicatorHeader,
    renderConversation,
    renderProjectAgentSettings,
    setMobileTab,
    startNewConversation,
    updateActiveProjectIndicators,
});
elements.contextIndicator?.addEventListener("click", openContextInspector);

function setMobileTab(tab) {
    const active = ["chat", "status", "projects", "filters"].includes(tab)
        ? tab
        : "chat";
    if (elements.desktop) {
        elements.desktop.dataset.mobileTab = active;
    }
    for (const button of elements.mobileTabs) {
        const selected = button.dataset.mobileTab === active;
        button.classList.toggle("active", selected);
        button.setAttribute("aria-pressed", String(selected));
    }
}

setMobileTab("chat");

for (const button of elements.mobileTabs) {
    button.addEventListener("click", () => {
        setMobileTab(button.dataset.mobileTab);
    });
}

function setTheme(theme) {
    const dark = theme === "dark";
    document.documentElement.dataset.theme = dark ? "dark" : "light";
    if (elements.themeToggle) {
        const label = dark
            ? "Activar fondo claro"
            : "Activar fondo oscuro";
        elements.themeToggle.textContent = dark ? "Claro" : "Oscuro";
        elements.themeToggle.setAttribute("aria-pressed", String(dark));
        elements.themeToggle.title = label;
        elements.themeToggle.setAttribute("aria-label", label);
    }
    window.localStorage.setItem(STORAGE_KEYS.theme, dark ? "dark" : "light");
}

setTheme(
    window.localStorage.getItem(STORAGE_KEYS.theme) === "dark" ? "dark" : "light"
);

elements.themeToggle?.addEventListener("click", () => {
    const dark = document.documentElement.dataset.theme === "dark";
    setTheme(dark ? "light" : "dark");
});

// El bloque "Agentes" (ajuste fino de contexto) queda oculto por defecto
// para no saturar la barra lateral: es un ajuste de uso ocasional, no el
// flujo principal de preguntar. Un toggle lo trae de vuelta cuando hace
// falta, y la preferencia se recuerda entre sesiones.

function setSidebarAdvanced(enabled) {
    if (elements.sidebar) {
        elements.sidebar.dataset.advanced = String(enabled);
    }
    if (elements.sidebarAdvancedToggle) {
        const label = enabled
            ? "Ocultar ajustes avanzados"
            : "Mostrar ajustes avanzados";
        elements.sidebarAdvancedToggle.setAttribute(
            "aria-pressed",
            String(enabled)
        );
        elements.sidebarAdvancedToggle.title = label;
        elements.sidebarAdvancedToggle.setAttribute("aria-label", label);
    }
    window.localStorage.setItem(STORAGE_KEYS.sidebarAdvanced, String(enabled));
}

setSidebarAdvanced(
    window.localStorage.getItem(STORAGE_KEYS.sidebarAdvanced) === "true"
);

elements.sidebarAdvancedToggle?.addEventListener("click", () => {
    const enabled =
        elements.sidebarAdvancedToggle.getAttribute("aria-pressed")
        !== "true";
    setSidebarAdvanced(enabled);
});

function supportsDialog(dialog) {
    return dialog && typeof dialog.showModal === "function";
}

function isAbortError(error) {
    return error?.name === "AbortError";
}

function setQueryRunning(running) {
    elements.askButton.disabled = running;
    elements.askButton.textContent = running ? "Consultando…" : "Preguntar";
    elements.stopQueryButton.hidden = !running;
    elements.stopQueryButton.disabled = false;
}

function askForText({
    title,
    label,
    value = "",
    confirmText = "Guardar",
}) {
    if (!supportsDialog(elements.textDialog)) {
        return Promise.resolve(window.prompt(title, value));
    }

    return new Promise((resolve) => {
        elements.textDialogTitle.textContent = title;
        elements.textDialogLabel.textContent = label;
        elements.textDialogInput.value = value;
        elements.textDialogConfirm.textContent = confirmText;
        elements.textDialog.returnValue = "";

        const cleanup = () => {
            elements.textDialogInput.removeEventListener(
                "keydown",
                onKeyDown
            );
        };
        const close = () => {
            cleanup();
            resolve(
                elements.textDialog.returnValue === "confirm"
                    ? elements.textDialogInput.value
                    : null
            );
        };
        const onKeyDown = (event) => {
            if (event.key === "Enter") {
                event.preventDefault();
                elements.textDialog.close("confirm");
            }
        };

        elements.textDialog.addEventListener("close", close, {once: true});
        elements.textDialogInput.addEventListener("keydown", onKeyDown);
        elements.textDialog.showModal();
        elements.textDialogInput.focus();
        elements.textDialogInput.select();
    });
}

function askForConfirmation({
    title,
    message,
    confirmText = "Confirmar",
}) {
    if (!supportsDialog(elements.confirmDialog)) {
        return Promise.resolve(window.confirm(message));
    }

    return new Promise((resolve) => {
        elements.confirmDialogTitle.textContent = title;
        elements.confirmDialogMessage.textContent = message;
        elements.confirmDialogConfirm.textContent = confirmText;
        elements.confirmDialog.returnValue = "";

        const close = () => {
            resolve(elements.confirmDialog.returnValue === "confirm");
        };

        elements.confirmDialog.addEventListener("close", close, {
            once: true,
        });
        elements.confirmDialog.showModal();
        elements.confirmDialogConfirm.focus();
    });
}

function updateCounts(stats) {
    elements.documentCount.textContent = stats.documents ?? 0;
    elements.chunkCount.textContent = stats.chunks ?? 0;
}

function updateChatProfile(data) {
    for (const profile of data.chat_profiles) {
        const option = elements.chatProfile.querySelector(
            `option[value="${profile.id}"]`
        );
        if (option) {
            option.textContent = `${profile.label} · ${profile.model}`;
        }
    }

    const activeProfile = data.chat_profiles.find(
        (profile) => profile.id === data.active_chat_profile
    );
    if (activeProfile) {
        elements.customChatProfile.hidden = true;
        elements.chatProfile.value = activeProfile.id;
        applyContextCeiling(data.model_context);
        return;
    }

    const currentModel = data.ollama.chat_model.name;
    elements.customChatProfile.hidden = false;
    elements.customChatProfile.textContent =
        `Personalizado · ${currentModel}`;
    elements.chatProfile.value = "custom";
    applyContextCeiling(data.model_context);
}

function applyContextCeiling(modelContext) {
    // Un modelo no avisa si le pides más contexto del que se entrenó:
    // degrada la respuesta en silencio. Se marca el techo real (perfil,
    // Ollama o el fallback prudente) en la propia interfaz para que no
    // haga falta saberlo de memoria.
    const inputs = [
        elements.verifierContextTokens,
        elements.writerContextTokens,
    ];
    const ceiling = Number(modelContext?.model_context_window) || 32768;

    let excedido = false;
    for (const input of inputs) {
        if (!input) {
            continue;
        }
        input.max = String(ceiling);
        if (Number(input.value) > ceiling) {
            excedido = true;
        }
    }

    if (!elements.contextCeilingHint) {
        return;
    }
    if (!modelContext || !modelContext.verified) {
        elements.contextCeilingHint.textContent =
            `Techo de contexto sin verificar; se aplica un límite ` +
            `prudente de ${formatContextWindow(ceiling)} tokens.`;
        elements.contextCeilingHint.hidden = false;
        return;
    }
    if (excedido) {
        elements.contextCeilingHint.textContent =
            `El modelo activo admite ${formatContextWindow(ceiling)} ` +
            `tokens (${modelContext.source}). Los valores por encima se ` +
            "recortarán automáticamente al responder.";
        elements.contextCeilingHint.hidden = false;
        return;
    }
    elements.contextCeilingHint.hidden = true;
}

function formatTokenCount(value) {
    // Sin la palabra "tokens": la etiqueta <small> ya la muestra debajo
    // del campo, y repetirla aquí sólo añade ruido visual.
    const number = Number(value);
    if (!Number.isFinite(number) || number <= 0) {
        return "";
    }
    return number % 1024 === 0 ? `${number / 1024}K` : String(number);
}

function updateContextValueReadout(input, output) {
    if (!input || !output) {
        return;
    }
    output.textContent = formatTokenCount(input.value);
}

function markActiveConversation() {
    for (const item of elements.conversationList.querySelectorAll(
        ".conversation-item"
    )) {
        item.classList.toggle(
            "active",
            item.dataset.conversationId === state.activeConversationId
        );
    }
}

function markActiveProject() {
    for (const item of elements.projectList.querySelectorAll(
        ".project-item"
    )) {
        item.classList.toggle(
            "active",
            item.dataset.projectId === state.activeProjectId
        );
    }
}

function updateDeleteProjectButton() {
    const canDelete =
        Boolean(state.activeProjectId) && state.activeProjectId !== state.defaultProjectId;
    elements.deleteProjectButton.disabled = !canDelete;
    elements.deleteProjectButton.title = canDelete
        ? `Eliminar proyecto ${state.activeProjectName || "seleccionado"}`
        : "El proyecto General no se puede eliminar";
}

function updateActiveProjectIndicators(projects = []) {
    const activeProject = projects.find(
        (project) => project.id === state.activeProjectId
    );
    if (activeProject) {
        state.activeProjectName = activeProject.name;
    }
    const label = state.activeProjectName || "Sin proyecto";
    elements.workspaceActiveProject.textContent = label;
    elements.workspaceActiveProject.title = label;
    updateDeleteProjectButton();
}

const AGENT_USAGE_ELEMENTS = {
    verifier: () => [
        elements.verifierContextUsage,
        elements.verifierContextBar,
    ],
    writer: () => [elements.writerContextUsage, elements.writerContextBar],
};

function renderAgentUsage(role, usage) {
    const latest = usage?.latest;
    const percent = latest ? Number(latest.usage_percent) : NaN;
    const [usageElement, barElement] = AGENT_USAGE_ELEMENTS[role]();
    const usedPercent = Number.isFinite(percent)
        ? Math.min(100, Math.max(0, percent))
        : NaN;
    const remainingPercent = Number.isFinite(usedPercent)
        ? 100 - usedPercent
        : NaN;
    usageElement.textContent = Number.isFinite(percent)
        ? `${formatContextPercent(remainingPercent)} disponible`
        : "Sin datos";
    usageElement.dataset.level = contextLevel(percent);
    barElement.style.width = Number.isFinite(usedPercent)
        ? `${usedPercent}%`
        : "0%";
    barElement.dataset.level = contextLevel(percent);
    usageElement.title = usage?.samples
        ? `Usado ${formatContextPercent(percent)} · ` +
          `${latest.total_tokens}/${latest.context_window_tokens} tokens. ` +
          `Máximo ${formatContextPercent(usage.maximum_percent)} · ` +
          `media ${formatContextPercent(usage.average_percent)}.`
        : "Todavía no hay consultas registradas para este agente.";
}

function setAgentTogglesEnabled(enabled) {
    // Viven en el panel de Opciones, fuera del bloque que se oculta al
    // no haber proyecto: se deshabilitan en lugar de desaparecer, para
    // que el panel no cambie de tamaño al cambiar de proyecto.
    elements.projectVerificationEnabled.disabled = !enabled;
    elements.projectMemoryEnabled.disabled = !enabled;
}

function renderProjectAgentSettings(project) {
    if (!project) {
        elements.projectAgentSettings.hidden = true;
        setAgentTogglesEnabled(false);
        return;
    }
    setAgentTogglesEnabled(true);
    const settings = project.agent_settings || {};
    const usage = project.context_usage || {};
    const memory = project.memory || {};
    elements.projectAgentSettings.hidden = false;
    elements.projectVerificationEnabled.checked =
        settings.verification_enabled !== false;
    elements.projectMemoryEnabled.checked =
        settings.project_memory_enabled !== false;
    // Las ventanas de contexto de los agentes son fijas: se muestran para
    // referencia pero el usuario no puede cambiarlas (los inputs están
    // deshabilitados en la plantilla y no se reactivan aquí).
    elements.verifierContextTokens.value =
        settings.verifier_context_tokens ?? 8192;
    elements.writerContextTokens.value =
        settings.writer_context_tokens ?? 16384;
    updateContextValueReadout(
        elements.verifierContextTokens,
        elements.verifierContextValue
    );
    updateContextValueReadout(
        elements.writerContextTokens,
        elements.writerContextValue
    );
    elements.projectContextOverall.textContent =
        usage.latest_percent == null
            ? "Sin uso registrado"
            : `Última consulta: ${formatContextPercent(
                usage.latest_percent
            )}`;
    renderAgentUsage("verifier", usage.verifier);
    renderAgentUsage("writer", usage.writer);
    if (!elements.projectMemoryEnabled.checked) {
        elements.projectMemorySummaryStatus.textContent =
            "Memoria compartida desactivada; el resumen se conserva.";
    } else if (memory.last_error) {
        elements.projectMemorySummaryStatus.textContent =
            "La última actualización de memoria falló.";
    } else if (memory.has_summary) {
        elements.projectMemorySummaryStatus.textContent =
            `${memory.summarized_turns || 0} turnos resumidos · ` +
            `${memory.pending_turns || 0} pendientes.`;
    } else {
        elements.projectMemorySummaryStatus.textContent =
            `${memory.pending_turns || 0} turnos pendientes para el ` +
            "primer resumen compartido.";
    }
}



function getSelectedTags() {
    return Array.from(
        elements.tagFilter.querySelectorAll('input[type="checkbox"]:checked')
    ).map((input) => input.value);
}

function updateTagFilterSummary() {
    const count = getSelectedTags().length;
    elements.tagFilterSummary.textContent = !count
        ? "Sin filtro"
        : count === 1
          ? "1 seleccionada"
          : `${count} seleccionadas`;
    elements.tagFilterClear.hidden = count === 0;
}

function filterTagOptions() {
    const query = elements.tagFilterSearch.value.trim().toLocaleLowerCase();
    const options = Array.from(
        elements.tagFilter.querySelectorAll(".tag-filter-option")
    );
    let visible = 0;
    for (const option of options) {
        const matches = option.dataset.tag.includes(query);
        option.hidden = !matches;
        if (matches) {
            visible += 1;
        }
    }
    elements.tagFilterEmpty.hidden = visible !== 0 || query.length === 0;
}

function clearTagFilter() {
    for (const input of elements.tagFilter.querySelectorAll(
        'input[type="checkbox"]:checked'
    )) {
        input.checked = false;
    }
    updateTagFilterSummary();
}

function createTagOption({tag, documents}, selected) {
    const option = document.createElement("label");
    option.className = "tag-filter-option";
    option.dataset.tag = tag.toLocaleLowerCase();

    const input = document.createElement("input");
    input.type = "checkbox";
    input.value = tag;
    input.checked = selected;
    input.addEventListener("change", updateTagFilterSummary);

    const name = document.createElement("span");
    name.textContent = `#${tag}`;
    const count = document.createElement("span");
    count.className = "tag-filter-option-count";
    count.textContent = Number.isFinite(documents) ? String(documents) : "";
    count.hidden = !Number.isFinite(documents);
    option.append(input, name, count);
    return option;
}

function updateTagFilter(tags) {
    if (!Array.isArray(tags)) {
        return;
    }
    const selected = new Set(getSelectedTags());
    const normalizedTags = new Map();
    for (const entry of tags) {
        const tag = typeof entry === "string" ? entry : entry?.tag;
        if (typeof tag !== "string" || !tag) {
            continue;
        }
        const documents = Number(entry?.documents);
        normalizedTags.set(tag, {
            tag,
            documents: Number.isFinite(documents) ? documents : NaN,
        });
    }
    const tagOptions = [...normalizedTags.values()].sort((left, right) =>
        left.tag.localeCompare(right.tag, "es")
    );
    elements.tagFilter.replaceChildren(
        ...tagOptions.map((option) =>
            createTagOption(option, selected.has(option.tag))
        )
    );
    const hasTags = tagOptions.length > 0;
    elements.tagFilterControl.hidden = false;
    elements.tagFilterSearch.disabled = !hasTags;
    elements.tagFilterSearch.placeholder = hasTags
        ? "Buscar etiquetas"
        : "No hay etiquetas indexadas";
    elements.tagFilterSearch.value = "";
    elements.tagFilterEmpty.textContent = hasTags
        ? "No hay etiquetas que coincidan."
        : "Todavía no hay etiquetas indexadas.";
    elements.tagFilterEmpty.hidden = hasTags;
    updateTagFilterSummary();
    if (!hasTags) {
        elements.tagFilterSummary.textContent = "Sin etiquetas";
    }
}

elements.tagFilterSearch.addEventListener("input", filterTagOptions);
elements.tagFilterClear.addEventListener("click", clearTagFilter);

function updateGraphHealth(graph) {
    // Enlaces rotos, notas huérfanas y alias ambiguos ya los calcula el
    // backend en cada reindexado (app.db.links.get_document_graph_status);
    // aquí sólo se traduce a un aviso legible cuando hay algo que revisar.
    if (!elements.graphHealthHint) {
        return;
    }
    if (!graph?.available) {
        elements.graphHealthHint.hidden = true;
        return;
    }

    const issues = [];
    const broken = Number(graph.broken_edges ?? 0);
    const orphans = Number(graph.orphan_documents ?? 0);
    const ambiguous = Number(graph.ambiguous_aliases ?? 0);
    if (broken > 0) {
        issues.push(
            `${broken} enlace${broken === 1 ? "" : "s"} [[...]] roto${
                broken === 1 ? "" : "s"
            }`
        );
    }
    if (orphans > 0) {
        issues.push(
            `${orphans} nota${orphans === 1 ? "" : "s"} sin conexiones`
        );
    }
    if (ambiguous > 0) {
        issues.push(
            `${ambiguous} alias ambiguo${ambiguous === 1 ? "" : "s"}`
        );
    }

    if (!issues.length) {
        elements.graphHealthHint.hidden = true;
        return;
    }
    elements.graphHealthHint.textContent =
        `Grafo de enlaces: ${issues.join(", ")}. Revisa los wikienlaces ` +
        "y los títulos duplicados del vault.";
    elements.graphHealthHint.hidden = false;
}

function updateStatusFilter(statuses) {
    // El vocabulario de estados lo define el vault del cliente, no una
    // lista escrita a mano en el HTML.
    if (!Array.isArray(statuses) || !statuses.length) {
        return;
    }
    const previous = elements.statusFilter.value;
    elements.statusFilter.replaceChildren();

    const all = document.createElement("option");
    all.value = "";
    all.textContent = "Todos";
    elements.statusFilter.append(all);

    for (const entry of statuses) {
        const option = document.createElement("option");
        option.value = entry.value;
        const label =
            entry.value.charAt(0).toUpperCase() + entry.value.slice(1);
        option.textContent = `${label} (${entry.documents})`;
        elements.statusFilter.append(option);
    }

    const stillAvailable = statuses.some(
        (entry) => entry.value === previous
    );
    elements.statusFilter.value = stillAvailable ? previous : "";
}

// Capacidades opcionales que necesitan el botón de vault y el selector de
// perfil de chat para funcionar sin pedir algo que el servidor no sabe
// responder. Son una lista (no una condición fija) para poder decir cuál
// falta en vez de un genérico "versión anterior" cuando se añada una nueva.
const REQUIRED_STATUS_CAPABILITIES = [
    ["conversation_history", "historial de conversaciones"],
    ["projects", "proyectos"],
    ["conversation_memory", "memoria de conversación"],
    ["hybrid_search", "búsqueda híbrida"],
    ["project_agents", "agentes de proyecto"],
    ["project_context_usage", "uso de contexto por proyecto"],
    ["project_memory", "memoria de proyecto"],
];

async function loadStatus() {
    try {
        const data = await apiRequest("/api/status");
        if (elements.appVersion) {
            const version = data.build?.version || "dev";
            elements.appVersion.textContent = `Local ${version}`;
            elements.appVersion.title = [
                data.build?.commit,
                data.build?.date,
            ].filter(Boolean).join(" · ");
        }
        const stats = data.index?.stats ?? data.stats ?? {};
        updateCounts(stats);
        elements.vaultPath.textContent =
            data.vault_display_path ?? data.vault_path;
        const watch = data.index?.watch;
        if (watch?.enabled) {
            const labels = {
                up_to_date: "Indexación automática: al día",
                indexing: "Indexación automática: indexando…",
                pending: "Indexación automática: cambios pendientes…",
                deferred: "Indexación automática: en cola…",
                error: "Indexación automática: error",
            };
            elements.watchStatus.textContent =
                labels[watch.state] || "Indexación automática activa";
            elements.watchStatus.dataset.state = watch.state || "";
            elements.watchStatus.hidden = false;
        } else {
            elements.watchStatus.hidden = true;
        }

        // El desplegable de perfiles recorre "chat_profiles" y lee
        // "ollama.chat_model.name" sin comprobar nada: si el servidor no
        // manda esa forma exacta (versión muy antigua o incompatible), hay
        // que detenerse aquí para no lanzar una excepción a mitad de
        // función y dejar el resto del estado (filtros, salud del grafo...)
        // sin refrescar.
        const hasChatProfileShape =
            Boolean(data.index)
            && Boolean(data.ollama?.chat_model)
            && Array.isArray(data.chat_profiles);
        if (!hasChatProfileShape) {
            elements.selectVaultButton.disabled = true;
            elements.chatProfile.disabled = true;
            elements.systemMessage.textContent =
                "El servidor está ejecutando una versión anterior o " +
                "incompatible. Cierra la ventana de iniciar_windows.bat, " +
                "vuelve a ejecutarlo y recarga esta página.";
            return null;
        }

        // El resto de capacidades son señales de compatibilidad para el
        // vault y el perfil de chat, no requisitos para evitar un fallo
        // aquí mismo: si falta alguna sólo se desactivan esos dos
        // controles y se dice cuál es, en vez de dejar de actualizar toda
        // la demás información del estado (lo que antes ocurría con un
        // simple "return null").
        const missingCapabilities = REQUIRED_STATUS_CAPABILITIES
            .filter(([key]) => data.capabilities?.[key] !== true)
            .map(([, label]) => label);
        const statusCompatible = missingCapabilities.length === 0;
        if (!statusCompatible) {
            console.error(
                "El servidor no ofrece estas capacidades:",
                missingCapabilities
            );
        }
        elements.selectVaultButton.disabled = !statusCompatible;
        elements.chatProfile.disabled = !statusCompatible;
        updateChatProfile(data);
        updateStatusFilter(data.document_statuses);
        updateTagFilter(
            data.retrieval?.vector_store?.tag_counts
            ?? data.retrieval?.vector_store?.tags
        );
        updateGraphHealth(data.retrieval?.graph);
        const graphConfigured =
            data.retrieval?.graph?.configured === true;
        elements.expandLinks.disabled = graphConfigured;
        if (graphConfigured) {
            elements.expandLinks.checked = false;
            elements.expandLinksLabel.textContent =
                "Seguir enlaces [[...]] (sustituido por el grafo)";
        } else {
            elements.expandLinksLabel.textContent =
                "Seguir enlaces [[...]]";
        }
        state.streamingSupported = data.capabilities?.streaming !== false;
        state.nativeVaultPickerAvailable =
            data.capabilities?.native_vault_picker === true;
        state.memorySummaryInterval =
            data.memory?.summary_interval ?? state.memorySummaryInterval;
        state.memoryRecentTurns =
            data.memory?.recent_turns ?? state.memoryRecentTurns;
        if (!state.activeConversationId) {
            updateMemoryStatus({enabled: elements.useMemory.checked});
        }
        elements.systemMessage.textContent = statusCompatible
            ? ""
            : "El servidor no ofrece todavía: "
                + `${missingCapabilities.join(", ")}. `
                + "Cierra la ventana de iniciar_windows.bat, vuelve a "
                + "ejecutarlo y recarga esta página.";
        state.lastStatusData = data;
        refreshContextIndicatorHeader();
        return data;
    } catch (error) {
        elements.systemMessage.textContent = error.message;
        return null;
    }
}

elements.historyDrawer?.addEventListener("toggle", () => {
    // Un cajón abierto sobre el hilo estorba al escribir: se cierra al
    // enviar una pregunta.
    if (elements.historyDrawer.open) {
        elements.question.blur();
    }
});

elements.newConversationButton.addEventListener(
    "click",
    startNewConversation
);

elements.useMemory.addEventListener("change", async () => {
    const enabled = elements.useMemory.checked;
    if (!state.activeConversationId) {
        window.localStorage.setItem(
            STORAGE_KEYS.memoryPreference,
            String(enabled)
        );
        updateMemoryStatus({enabled});
        return;
    }

    elements.useMemory.disabled = true;
    elements.memoryStatus.textContent = "Guardando preferencia de memoria…";
    try {
        const result = await apiRequest(
            `/api/conversations/${encodeURIComponent(
                state.activeConversationId
            )}/memory`,
            {
                method: "PATCH",
                body: JSON.stringify({enabled}),
            }
        );
        window.localStorage.setItem(
            STORAGE_KEYS.memoryPreference,
            String(enabled)
        );
        updateMemoryStatus(result.memory);
    } catch (error) {
        elements.useMemory.checked = !enabled;
        elements.memoryStatus.textContent = error.message;
    } finally {
        elements.useMemory.disabled = false;
    }
});

elements.newProjectButton.addEventListener("click", async () => {
    const requested = await askForText({
        title: "Nuevo proyecto",
        label: "Nombre del proyecto",
        confirmText: "Crear",
    });
    const name = requested?.trim();
    if (!name) {
        return;
    }
    try {
        const project = await apiRequest("/api/projects", {
            method: "POST",
            body: JSON.stringify({name}),
        });
        state.activeProjectId = project.id;
        state.activeProjectName = project.name;
        await loadProjects({loadThreads: true});
    } catch (error) {
        elements.queryStatus.textContent = error.message;
    }
});

elements.deleteProjectButton.addEventListener("click", async () => {
    if (!state.activeProjectId || state.activeProjectId === state.defaultProjectId) {
        return;
    }
    const project = state.loadedProjects.find(
        (candidate) => candidate.id === state.activeProjectId
    );
    const projectName = project?.name || state.activeProjectName || "este proyecto";
    const conversations = Number(project?.conversation_count || 0);
    const conversationLabel =
        conversations === 1
            ? "1 conversacion"
            : `${conversations} conversaciones`;
    const confirmed = await askForConfirmation({
        title: "Eliminar proyecto",
        message:
            `Se eliminara "${projectName}". ` +
            `Sus ${conversationLabel} pasaran a General.`,
        confirmText: "Eliminar",
    });
    if (!confirmed) {
        return;
    }

    elements.deleteProjectButton.disabled = true;
    try {
        const result = await apiRequest(
            `/api/projects/${encodeURIComponent(state.activeProjectId)}`,
            {method: "DELETE"}
        );
        state.activeProjectId = result.fallback_project_id ?? state.defaultProjectId;
        state.activeProjectName = "";
        state.activeConversationId = null;
        state.activeConversationTitle = "";
        window.localStorage.setItem(STORAGE_KEYS.activeProject, state.activeProjectId);
        elements.chatThread.replaceChildren();
        elements.useMemory.checked = preferredMemoryEnabled();
        updateMemoryStatus({enabled: elements.useMemory.checked});
        setChatLayout(false);
        await loadProjects({loadThreads: false});
        await loadConversations({selectLatest: false});
        elements.queryStatus.textContent =
            result.moved_conversations > 0
                ? `Proyecto eliminado. ${result.moved_conversations} ` +
                  "conversaciones movidas a General."
                : "Proyecto eliminado.";
    } catch (error) {
        elements.queryStatus.textContent = error.message;
    } finally {
        updateDeleteProjectButton();
    }
});

elements.saveProjectAgentSettings.addEventListener("click", async () => {
    if (!state.activeProjectId) {
        return;
    }
    elements.saveProjectAgentSettings.disabled = true;
    elements.saveProjectAgentSettings.textContent = "Guardando…";
    // Las ventanas de contexto no se editan desde la UI: se reenvían tal cual
    // las devolvió el servidor para el proyecto activo, de modo que este
    // guardado solo cambia los interruptores de verificación y memoria.
    const currentProject = state.loadedProjects.find(
        (candidate) => candidate.id === state.activeProjectId
    );
    const currentSettings = currentProject?.agent_settings || {};
    try {
        await apiRequest(
            `/api/projects/${encodeURIComponent(
                state.activeProjectId
            )}/agent-settings`,
            {
                method: "PATCH",
                body: JSON.stringify({
                    verification_enabled:
                        elements.projectVerificationEnabled.checked,
                    project_memory_enabled:
                        elements.projectMemoryEnabled.checked,
                    verifier_context_tokens:
                        Number(currentSettings.verifier_context_tokens) || 8192,
                    writer_context_tokens:
                        Number(currentSettings.writer_context_tokens) || 16384,
                }),
            }
        );
        await loadProjects({loadThreads: false});
        elements.systemMessage.textContent =
            "Configuración de agentes guardada para el proyecto activo.";
    } catch (error) {
        elements.systemMessage.textContent = error.message;
    } finally {
        elements.saveProjectAgentSettings.disabled = false;
        elements.saveProjectAgentSettings.textContent = "Guardar cambios";
    }
});

elements.chatProfile.addEventListener("change", async () => {
    const profileId = elements.chatProfile.value;
    if (profileId === "custom") {
        return;
    }
    elements.chatProfile.disabled = true;
    elements.systemMessage.textContent = "Guardando perfil de respuesta…";
    try {
        const result = await apiRequest("/api/config/chat-profile", {
            method: "POST",
            body: JSON.stringify({profile: profileId}),
        });
        const status = await loadStatus();
        const installed = status?.ollama?.chat_model?.installed;
        const label = result.profile.label;
        const model = result.profile.model;
        // Que Ollama esté parado no es un problema del perfil elegido: ya
        // se ve en el resto de la interfaz en cuanto se intenta preguntar.
        // Repetirlo aquí sólo añade ruido. Que falte el modelo sí se avisa,
        // porque sin descargarlo la consulta fallará seguro.
        if (installed === false) {
            elements.systemMessage.textContent =
                `Perfil ${label} guardado, pero falta el modelo local. ` +
                `Ejecuta: ollama pull ${model}`;
        } else {
            elements.systemMessage.textContent = "";
        }
    } catch (error) {
        await loadStatus();
        elements.systemMessage.textContent = error.message;
    } finally {
        elements.chatProfile.disabled = false;
    }
});

elements.selectVaultButton.addEventListener("click", async () => {
    const current = elements.vaultPath.textContent.trim();
    elements.selectVaultButton.disabled = true;
    elements.selectVaultButton.textContent = "Eligiendo…";
    try {
        let entered;
        if (state.nativeVaultPickerAvailable) {
            const selection = await apiRequest("/api/vault/pick", {
                method: "POST",
                body: JSON.stringify({}),
            });
            entered = selection.selected ? selection.vault_path : null;
        } else {
            entered = await askForText({
                title: "Elegir vault",
                label: "Ruta de la carpeta del vault de Obsidian",
                value: current,
                confirmText: "Usar esta carpeta",
            });
        }
        const target = entered?.trim();
        if (!target || target === current) {
            elements.systemMessage.textContent =
                "No se cambió la carpeta del vault.";
            return;
        }
        elements.selectVaultButton.textContent = "Cambiando…";
        const result = await apiRequest("/api/vault/select", {
            method: "POST",
            body: JSON.stringify({vault_path: target}),
        });
        if (!result.selected) {
            elements.systemMessage.textContent =
                "No se cambió la carpeta del vault.";
            return;
        }
        elements.vaultPath.textContent =
            result.vault_display_path ?? result.vault_path;
        startNewConversation();
        await loadStatus();
        await loadProjects({loadThreads: true});
        elements.systemMessage.textContent =
            "Vault actualizado. Los proyectos y conversaciones mostrados " +
            "ahora corresponden a este vault. Pulsa «Indexar documentos» " +
            "para reconstruir o actualizar el índice.";
    } catch (error) {
        elements.systemMessage.textContent = error.message;
    } finally {
        elements.selectVaultButton.disabled = false;
        elements.selectVaultButton.textContent = "Elegir vault";
    }
});

function describeIndexResult(result) {
    const parts = [
        `${result.indexed} actualizados`,
        `${result.unchanged} sin cambios`,
        `${result.deleted} eliminados`,
        `${result.chunks_created} fragmentos nuevos`,
    ];
    if (result.errors.length) {
        parts.push(`${result.errors.length} errores`);
        parts.push(result.errors[result.errors.length - 1]);
        console.error(result.errors);
    }
    if (result.rebuilt) {
        parts.push(
            `índice reconstruido: ${result.rebuild_reasons.join(", ")}`
        );
    }
    return parts.join(" · ");
}

function describeIndexProgress(progress) {
    if (progress.phase === "unchanged" || progress.phase === "embedding") {
        const position = progress.total
            ? `${progress.processed} de ${progress.total}`
            : `${progress.processed}`;
        const file = progress.current_file
            ? ` · ${progress.current_file}`
            : "";
        return `Indexando ${position} archivos${file}`;
    }
    if (progress.phase === "scanning") {
        return "Explorando el vault…";
    }
    if (progress.phase === "committing") {
        return "Activando el índice preparado…";
    }
    if (progress.phase === "cancelling") {
        return "Cancelando la indexación…";
    }
    return "Preparando la indexación…";
}

async function pollIndexingProgress() {
    while (true) {
        await new Promise((resolve) => setTimeout(resolve, 400));
        let progress;
        try {
            progress = await apiRequest("/api/index/progress");
        } catch (error) {
            throw new Error(
                "Se perdió el contacto con el servidor durante la " +
                "indexación."
            );
        }

        if (progress.running) {
            elements.systemMessage.textContent =
                describeIndexProgress(progress);
            continue;
        }
        if (progress.error) {
            throw new Error(progress.error);
        }
        if (progress.cancelled || progress.phase === "cancelled") {
            throw new Error("Indexación cancelada por el usuario.");
        }
        if (progress.result) {
            return progress.result;
        }
        throw new Error("La indexación terminó sin resultado.");
    }
}

elements.indexButton.addEventListener("click", async () => {
	if (state.indexingActive) {
		elements.indexButton.disabled = true;
		elements.indexButton.textContent = "Cancelando…";
		try {
			await apiRequest("/api/index/cancel", {
				method: "POST",
				body: JSON.stringify({}),
			});
			elements.systemMessage.textContent = "Cancelando la indexación…";
		} catch (error) {
			elements.systemMessage.textContent = error.message;
		}
		return;
	}
	state.indexingActive = true;
	elements.indexButton.disabled = false;
	elements.indexButton.textContent = "Cancelar indexación";
    elements.systemMessage.textContent = "Preparando la indexación…";
    try {
        await apiRequest("/api/index/start", {
            method: "POST",
            body: JSON.stringify({}),
        });
        const result = await pollIndexingProgress();
        updateCounts(result.stats);
        elements.systemMessage.textContent = describeIndexResult(result);
        await loadStatus();
    } catch (error) {
        elements.systemMessage.textContent = error.message;
    } finally {
		state.indexingActive = false;
        elements.indexButton.disabled = false;
        elements.indexButton.textContent = "Indexar documentos";
    }
});

elements.stopQueryButton.addEventListener("click", () => {
    if (!state.activeQueryController) {
        return;
    }
    elements.stopQueryButton.disabled = true;
    elements.queryStatus.textContent = "Deteniendo consulta…";
    state.activeQueryController.abort();
});

elements.question.addEventListener("keydown", (event) => {
    // Enter envía la pregunta; Mayús+Enter sigue insertando un salto de
    // línea. isComposing evita enviar a medias mientras un IME (chino,
    // japonés, coreano...) todavía está componiendo el carácter.
    if (event.key !== "Enter" || event.shiftKey || event.isComposing) {
        return;
    }
    event.preventDefault();
    if (elements.askButton.disabled) {
        return;
    }
    if (typeof elements.form.requestSubmit === "function") {
        elements.form.requestSubmit(elements.askButton);
    } else {
        elements.askButton.click();
    }
});

elements.form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const question = elements.question.value.trim();
    if (!question) {
        return;
    }

    closeHistoryDrawer();
    setMobileTab("chat");
    state.activeQueryController = new AbortController();
    setQueryRunning(true);
    elements.queryStatus.textContent = "Consultando la documentación…";
    elements.question.value = "";

    const body = {
        question,
        top_k: Number(elements.topK.value),
        status: elements.statusFilter.value || null,
        vigencia: elements.vigenciaFilter.value || null,
        tags: getSelectedTags(),
        expand_links: elements.expandLinks.checked,
        conversation_id: state.activeConversationId,
        project_id: state.activeProjectId,
        use_memory: elements.useMemory.checked,
    };

    let pendingTurn = null;

    try {
        let result = null;

        if (state.streamingSupported) {
            result = await streamQuery(
                question,
                body,
                state.activeQueryController.signal
            );
        }

        if (result === null) {
            // Vía clásica: sin streaming o no disponible en el servidor.
            pendingTurn = createTurnElement(
                {question, sources: []},
                {pending: true}
            );
            elements.chatThread.append(pendingTurn);
            setChatLayout(
                true,
                state.activeConversationTitle || "Nueva conversación"
            );
            scrollToLatestTurn();

            result = await apiRequest("/api/query", {
                method: "POST",
                body: JSON.stringify(body),
                signal: state.activeQueryController.signal,
            });
            const completedTurn = createTurnElement({
                id: result.turn_id,
                question,
                answer: result.answer,
                sources: result.sources,
                chat_model: result.chat_model,
                agents: result.agents,
                retrieval: result.retrieval,
            });
            pendingTurn.replaceWith(completedTurn);
            pendingTurn = null;
        }

        elements.stopQueryButton.hidden = true;
        state.activeConversationId = result.conversation_id;
        await loadConversations();
        await loadProjects({loadThreads: false});
        setChatLayout(true, state.activeConversationTitle);
        updateMemoryStatus(result.memory);
        state.latestTurnMaxUsagePercent = maxUsagePercentFromAgents(result.agents);
        refreshContextIndicatorHeader();

        const warnings = [
            result.agents?.warning,
            result.memory?.warning,
            result.project_memory?.warning,
        ].filter(Boolean);
        elements.queryStatus.textContent = warnings.length
            ? `Consulta terminada con avisos: ${warnings.join(" ")}`
            : "Consulta terminada.";
        scrollToLatestTurn();
    } catch (error) {
        const stopped = isAbortError(error);
        const failedTurn = createTurnElement(
            {question, sources: []},
            {
                error: stopped
                    ? "Consulta detenida por el usuario."
                    : error.message,
            }
        );
        if (pendingTurn) {
            pendingTurn.replaceWith(failedTurn);
        } else {
            elements.chatThread.append(failedTurn);
        }
        setChatLayout(true, state.activeConversationTitle || "Nueva conversación");
        elements.queryStatus.textContent = stopped
            ? "Consulta detenida."
            : "No se pudo completar la consulta.";
        scrollToLatestTurn();
    } finally {
        state.activeQueryController = null;
        setQueryRunning(false);
        elements.question.focus();
    }
});

Promise.all([
    loadStatus(),
    loadProjects({loadThreads: true}),
]);
