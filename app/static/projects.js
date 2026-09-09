"use strict";

import {request as apiRequest} from "./api.js";
import {formatContextPercent} from "./context-inspector.js";
import {state, STORAGE_KEYS} from "./state.js";

export function createProjectsController({
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
}) {
    async function selectProject(
        projectId,
        projectName,
        {selectLatest = true} = {}
    ) {
        if (!projectId) {
            return;
        }
        state.activeProjectId = projectId;
        state.activeProjectName = projectName || "";
        window.localStorage.setItem(STORAGE_KEYS.activeProject, projectId);
        markActiveProject();
        updateActiveProjectIndicators();
        renderProjectAgentSettings(
            state.loadedProjects.find((project) => project.id === projectId)
        );
        startNewConversation();
        await loadConversations({selectLatest});
    }

    function createProjectListItem(project) {
        const item = document.createElement("div");
        item.className = "project-item";
        item.dataset.projectId = project.id;

        const openButton = document.createElement("button");
        openButton.type = "button";
        openButton.className = "project-open";
        openButton.title = project.name;
        const name = document.createElement("span");
        name.textContent = project.name;
        const count = document.createElement("small");
        count.textContent =
            `${project.conversation_count} ` +
            (project.conversation_count === 1
                ? "conversación"
                : "conversaciones");
        const context = document.createElement("small");
        context.className = "project-context-usage";
        const latestPercent = project.context_usage?.latest_percent;
        context.textContent = latestPercent == null
            ? "Contexto sin datos"
            : `Contexto ${formatContextPercent(latestPercent)}`;
        context.title = project.context_usage?.maximum_percent == null
            ? "Todavía no hay consultas con telemetría de contexto."
            : `Máximo registrado ${formatContextPercent(
                project.context_usage.maximum_percent
            )} · media ${formatContextPercent(
                project.context_usage.average_percent
            )}`;
        openButton.append(name, count, context);
        openButton.addEventListener(
            "click",
            () => selectProject(project.id, project.name)
        );

        const renameButton = document.createElement("button");
        renameButton.type = "button";
        renameButton.className = "project-rename";
        renameButton.textContent = "✎";
        renameButton.title = "Renombrar proyecto";
        renameButton.setAttribute(
            "aria-label",
            `Renombrar proyecto ${project.name}`
        );
        renameButton.addEventListener("click", async () => {
            const requested = await askForText({
                title: "Renombrar proyecto",
                label: "Nombre del proyecto",
                value: project.name,
                confirmText: "Renombrar",
            });
            const newName = requested?.trim();
            if (!newName || newName === project.name) {
                return;
            }
            try {
                await apiRequest(
                    `/api/projects/${encodeURIComponent(project.id)}`,
                    {
                        method: "PATCH",
                        body: JSON.stringify({name: newName}),
                    }
                );
                await loadProjects({loadThreads: false});
            } catch (error) {
                elements.queryStatus.textContent = error.message;
            }
        });

        item.append(openButton, renameButton);
        return item;
    }

    async function loadProjects({loadThreads = true} = {}) {
        try {
            const data = await apiRequest("/api/projects");
            const projects = data.projects || [];
            state.defaultProjectId =
                data.default_project_id ?? state.defaultProjectId;
            state.loadedProjects = projects;
            elements.projectList.replaceChildren();
            elements.projectListEmpty.classList.toggle(
                "hidden",
                projects.length > 0
            );
            projects.forEach((project) => {
                elements.projectList.append(createProjectListItem(project));
            });

            const savedProjectId = window.localStorage.getItem(
                STORAGE_KEYS.activeProject
            );
            const activeStillExists = projects.some(
                (project) => project.id === state.activeProjectId
            );
            const savedStillExists = projects.some(
                (project) => project.id === savedProjectId
            );
            if (!activeStillExists) {
                state.activeProjectId = savedStillExists
                    ? savedProjectId
                    : state.defaultProjectId ?? projects[0]?.id ?? null;
            }
            if (state.activeProjectId) {
                window.localStorage.setItem(
                    STORAGE_KEYS.activeProject,
                    state.activeProjectId
                );
            }
            markActiveProject();
            updateActiveProjectIndicators(projects);
            renderProjectAgentSettings(
                projects.find(
                    (project) => project.id === state.activeProjectId
                )
            );
            refreshContextIndicatorHeader();
            if (loadThreads && state.activeProjectId) {
                startNewConversation();
                await loadConversations({selectLatest: true});
            }
            return projects;
        } catch (error) {
            elements.projectListEmpty.textContent =
                `No se pudieron cargar los proyectos: ${error.message}`;
            elements.projectListEmpty.classList.remove("hidden");
            return [];
        }
    }

    function closeHistoryDrawer() {
        if (elements.historyDrawer) {
            elements.historyDrawer.open = false;
        }
    }

    async function openConversation(conversationId) {
        setMobileTab("chat");
        closeHistoryDrawer();
        try {
            const conversation = await apiRequest(
                `/api/conversations/${encodeURIComponent(conversationId)}`
            );
            renderConversation(conversation);
        } catch (error) {
            elements.queryStatus.textContent = error.message;
        }
    }

    function createConversationListItem(conversation) {
        const item = document.createElement("div");
        item.className = "conversation-item";
        item.dataset.conversationId = conversation.id;

        const openButton = document.createElement("button");
        openButton.type = "button";
        openButton.className = "conversation-open";
        openButton.title = conversation.title;
        const title = document.createElement("span");
        title.textContent = conversation.title;
        const count = document.createElement("small");
        count.textContent =
            `${conversation.turn_count} ` +
            (conversation.turn_count === 1 ? "consulta" : "consultas");
        openButton.append(title, count);
        openButton.addEventListener(
            "click",
            () => openConversation(conversation.id)
        );

        const deleteButton = document.createElement("button");
        deleteButton.type = "button";
        deleteButton.className = "conversation-delete";
        deleteButton.textContent = "×";
        deleteButton.title = "Borrar conversación";
        deleteButton.setAttribute(
            "aria-label",
            `Borrar conversación ${conversation.title}`
        );
        deleteButton.addEventListener("click", async () => {
            const confirmed = await askForConfirmation({
                title: "Borrar conversación",
                message:
                    "Esta conversación se eliminará del historial del proyecto.",
                confirmText: "Borrar",
            });
            if (!confirmed) {
                return;
            }
            try {
                await apiRequest(
                    `/api/conversations/${encodeURIComponent(
                        conversation.id
                    )}`,
                    {method: "DELETE"}
                );
                if (state.activeConversationId === conversation.id) {
                    startNewConversation();
                }
                await loadConversations({selectLatest: false});
                await loadProjects({loadThreads: false});
            } catch (error) {
                elements.queryStatus.textContent = error.message;
            }
        });

        item.append(openButton, deleteButton);
        return item;
    }

    async function loadConversations({
        selectLatest = false,
        append = false,
    } = {}) {
        if (!state.activeProjectId) {
            elements.conversationList.replaceChildren();
            return [];
        }
        try {
            const offset = append ? state.conversationOffset : 0;
            const data = await apiRequest(
                "/api/conversations?project_id=" +
                encodeURIComponent(state.activeProjectId) +
                `&limit=50&offset=${offset}`
            );
            const conversations = data.conversations || [];
            if (!append) {
                elements.conversationList.replaceChildren();
            }
            elements.conversationListEmpty.classList.toggle(
                "hidden",
                conversations.length > 0
            );
            conversations.forEach((conversation) => {
                elements.conversationList.append(
                    createConversationListItem(conversation)
                );
            });
            state.conversationOffset =
                data.pagination?.next_offset ?? offset + conversations.length;
            state.conversationHasMore = data.pagination?.has_more === true;
            elements.conversationLoadMore?.classList.toggle(
                "hidden",
                !state.conversationHasMore
            );

            const active = conversations.find(
                (conversation) =>
                    conversation.id === state.activeConversationId
            );
            if (active) {
                state.activeConversationTitle = active.title;
                elements.conversationTitle.textContent = active.title;
            }
            markActiveConversation();
            updateHistorySummary(
                data.pagination?.total ??
                    elements.conversationList.children.length
            );

            if (
                selectLatest &&
                !state.activeConversationId &&
                conversations.length > 0
            ) {
                await openConversation(conversations[0].id);
            }
            return conversations;
        } catch (error) {
            elements.conversationListEmpty.textContent =
                `No se pudo cargar el historial: ${error.message}`;
            elements.conversationListEmpty.classList.remove("hidden");
            return [];
        }
    }

    async function loadMoreConversations() {
        if (!state.conversationHasMore) {
            return;
        }
        elements.conversationLoadMore.disabled = true;
        await loadConversations({append: true});
        elements.conversationLoadMore.disabled = false;
    }

    function updateHistorySummary(total) {
        if (!elements.historyDrawer) {
            return;
        }
        const summary = elements.historyDrawer.querySelector("summary");
        if (summary) {
            summary.textContent = total ? `Historial (${total})` : "Historial";
        }
    }

    elements.conversationLoadMore?.addEventListener(
        "click",
        loadMoreConversations
    );

    return {
        closeHistoryDrawer,
        loadConversations,
        loadProjects,
        selectProject,
    };
}
