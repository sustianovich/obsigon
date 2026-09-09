"use strict";

import {APIError, errorFromResponse, request as apiRequest} from "./api.js";
import {maxUsagePercentFromAgents} from "./context-inspector.js";
import {readServerEvents} from "./sse.js";
import {
    createAssistantHeader,
    createCopyAnswerButton,
    createSourceCard,
    renderAnswer,
} from "./sources.js";
import {state, STORAGE_KEYS} from "./state.js";

const STAGE_LABELS = {
    decomposition: "Descomponiendo la pregunta…",
    research: "Investigando en el vault…",
    verifier: "Verificando las fuentes…",
    writer: "Redactando la respuesta…",
    verifying: "Verificando las fuentes…",
    writing: "Redactando la respuesta…",
};

export function createChatController({
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
}) {
    function createStreamingTurn(question) {
        const turnKey = `stream-${++state.pendingTurnNumber}`;
        const wrapper = document.createElement("article");
        wrapper.className = "chat-turn";
        wrapper.dataset.turnId = String(turnKey);
        const userRow = document.createElement("div");
        userRow.className = "message-row user-row";
        const userBubble = document.createElement("div");
        userBubble.className = "message-bubble user-bubble";
        userBubble.textContent = question;
        userRow.append(userBubble);
        const assistantRow = document.createElement("div");
        assistantRow.className = "message-row assistant-row";
        const assistantBody = document.createElement("div");
        assistantBody.className = "assistant-body";
        const assistantLabel = document.createElement("div");
        assistantLabel.className = "assistant-label";
        assistantLabel.textContent = "Obsidian RAG";
        const stage = document.createElement("div");
        stage.className = "stream-stage";
        stage.textContent = "Buscando en la documentación…";
        const answer = document.createElement("div");
        answer.className = "answer-card streaming-answer";
        answer.setAttribute("aria-busy", "true");
        const sourcesHolder = document.createElement("div");
        sourcesHolder.className = "stream-sources";
        assistantBody.append(
            createAssistantHeader(assistantLabel), stage, answer, sourcesHolder
        );
        assistantRow.append(assistantBody);
        wrapper.append(userRow, assistantRow);
        return {
            wrapper,
            turnKey,
            assistantBody,
            assistantLabel,
            stage,
            answer,
            sourcesHolder,
            text: "",
            sources: [],
        };
    }

    function renderSourcesInto(holder, sources, turnKey) {
        holder.replaceChildren();
        if (!sources.length) {
            return;
        }
        const group = document.createElement("details");
        group.className = "turn-sources";
        const summary = document.createElement("summary");
        summary.textContent = `${sources.length} fuentes documentales`;
        const grid = document.createElement("div");
        grid.className = "sources-grid";
        sources.forEach((source, index) => {
            grid.append(createSourceCard(source, index + 1, turnKey));
        });
        group.append(summary, grid);
        holder.append(group);
    }

    function isThreadNearBottom() {
        const thread = elements.chatThread;
        if (!thread) {
            return true;
        }
        return thread.scrollHeight - thread.scrollTop - thread.clientHeight < 120;
    }

    function createTurnElement(turn, options = {}) {
        const turnKey = turn.id ?? `pending-${++state.pendingTurnNumber}`;
        const wrapper = document.createElement("article");
        wrapper.className = "chat-turn";
        wrapper.dataset.turnId = String(turnKey);
        const userRow = document.createElement("div");
        userRow.className = "message-row user-row";
        const userBubble = document.createElement("div");
        userBubble.className = "message-bubble user-bubble";
        userBubble.textContent = turn.question;
        userRow.append(userBubble);
        const assistantRow = document.createElement("div");
        assistantRow.className = "message-row assistant-row";
        const assistantBody = document.createElement("div");
        assistantBody.className = "assistant-body";
        const assistantLabel = document.createElement("div");
        assistantLabel.className = "assistant-label";
        assistantLabel.textContent = turn.chat_model
            ? `Obsidian RAG · ${turn.chat_model}`
            : "Obsidian RAG";
        const answer = document.createElement("div");
        answer.className = "answer-card";
        if (options.pending) {
            answer.classList.add("loading-answer");
            answer.textContent = "Buscando en la documentación…";
        } else if (options.error) {
            answer.classList.add("error-answer");
            answer.textContent = options.error;
        } else {
            const sources = Array.isArray(turn.sources) ? turn.sources : [];
            const answerText = turn.answer || "";
            renderAnswer(answer, answerText, sources.length, turnKey);
            assistantBody.append(
                createAssistantHeader(assistantLabel, [
                    createCopyAnswerButton(() => answerText),
                ])
            );
            const agentTrace = createAgentTrace(turn);
            if (agentTrace) {
                assistantBody.append(agentTrace);
            }
            const retrievalTrace = createRetrievalTrace(turn);
            if (retrievalTrace) {
                assistantBody.append(retrievalTrace);
            }
            assistantBody.append(answer);
            if (sources.length) {
                const sourceGroup = document.createElement("details");
                sourceGroup.className = "turn-sources";
                const sourceSummary = document.createElement("summary");
                sourceSummary.textContent = `${sources.length} fuentes documentales`;
                const sourceGrid = document.createElement("div");
                sourceGrid.className = "sources-grid";
                sources.forEach((source, index) => {
                    sourceGrid.append(createSourceCard(source, index + 1, turnKey));
                });
                sourceGroup.append(sourceSummary, sourceGrid);
                assistantBody.append(sourceGroup);
                assistantRow.append(assistantBody);
                wrapper.append(userRow, assistantRow);
                return wrapper;
            }
        }
        if (!assistantLabel.parentElement) {
            assistantBody.append(createAssistantHeader(assistantLabel), answer);
        }
        assistantRow.append(assistantBody);
        wrapper.append(userRow, assistantRow);
        return wrapper;
    }

    async function streamQuery(question, body, signal) {
        const turn = createStreamingTurn(question);
        elements.chatThread.append(turn.wrapper);
        setChatLayout(true, state.activeConversationTitle || "Nueva conversación");
        scrollToLatestTurn();
        let response;
        try {
            response = await fetch("/api/query/stream", {
                method: "POST",
                headers: {"Content-Type": "application/json"},
                body: JSON.stringify(body),
                signal,
            });
        } catch (error) {
            turn.wrapper.remove();
            throw error;
        }
        if (!response.ok || !response.body) {
            turn.wrapper.remove();
            if (response.status === 409 || response.status === 404) {
                state.streamingSupported = false;
                return null;
            }
            const payload = await response.json().catch(() => ({}));
            throw errorFromResponse(response, payload);
        }
        let finalPayload = null;
        let failure = null;
        let atBottom = true;
        try {
            for await (const event of readServerEvents(response)) {
                if (event.name === "retrieval") {
                    turn.sources = event.data.sources || [];
                    turn.trace = event.data.trace;
                    renderSourcesInto(turn.sourcesHolder, turn.sources, turn.turnKey);
                    turn.stage.textContent = turn.sources.length
                        ? `${turn.sources.length} fuentes recuperadas`
                        : "Sin fuentes suficientes";
                    elements.queryStatus.textContent = turn.stage.textContent;
                    scrollToLatestTurn();
                } else if (event.name === "stage") {
                    turn.stage.textContent =
                        STAGE_LABELS[event.data.stage] || turn.stage.textContent;
                    elements.queryStatus.textContent = turn.stage.textContent;
                } else if (event.name === "delta") {
                    turn.text += event.data.text;
                    turn.answer.textContent = turn.text;
                    if (atBottom) {
                        scrollToLatestTurn();
                    }
                    atBottom = isThreadNearBottom();
                } else if (event.name === "done") {
                    finalPayload = event.data;
                } else if (event.name === "error") {
                    failure = event.data.detail || "La consulta no se completó.";
                }
            }
        } catch (error) {
            turn.wrapper.remove();
            throw error;
        }
        if (failure) {
            turn.wrapper.remove();
            throw new APIError(
                failure, response.headers.get("X-Request-ID"), response.status
            );
        }
        if (!finalPayload) {
            turn.wrapper.remove();
            throw new Error("La conexión se interrumpió antes de terminar.");
        }
        const completed = createTurnElement({
            id: finalPayload.turn_id,
            question,
            answer: finalPayload.answer,
            sources: turn.sources,
            chat_model: finalPayload.chat_model,
            agents: finalPayload.agents,
            retrieval: finalPayload.retrieval || {trace: turn.trace},
        });
        turn.wrapper.replaceWith(completed);
        return finalPayload;
    }

    function createLoadOlderTurnsButton() {
        const button = document.createElement("button");
        button.type = "button";
        button.className =
            "button secondary pagination-button load-older-turns";
        button.textContent = "Cargar consultas anteriores";
        button.addEventListener("click", async () => {
            if (!state.activeConversationId || !state.conversationHasOlderTurns) {
                return;
            }
            button.disabled = true;
            try {
                const page = await apiRequest(
                    `/api/conversations/${encodeURIComponent(
                        state.activeConversationId
                    )}?limit=100&before_id=${state.conversationNextBefore}`
                );
                const fragment = document.createDocumentFragment();
                for (const turn of page.turns || []) {
                    fragment.append(createTurnElement(turn));
                }
                button.replaceWith(fragment);
                state.conversationNextBefore =
                    page.turn_page?.next_before_id || 0;
                state.conversationHasOlderTurns =
                    page.turn_page?.has_more === true;
                if (state.conversationHasOlderTurns) {
                    elements.chatThread.prepend(createLoadOlderTurnsButton());
                }
            } catch (error) {
                button.disabled = false;
                elements.queryStatus.textContent = error.message;
            }
        });
        return button;
    }

    function renderConversation(conversation) {
        state.activeConversationId = conversation.id;
        state.activeConversationTitle = conversation.title;
        const memory = conversation.memory || {
            enabled: conversation.memory_enabled !== false,
        };
        elements.useMemory.checked = memory.enabled !== false;
        window.localStorage.setItem(
            STORAGE_KEYS.memoryPreference, String(elements.useMemory.checked)
        );
        updateMemoryStatus(memory);
        elements.chatThread.replaceChildren();
        state.conversationNextBefore =
            conversation.turn_page?.next_before_id || 0;
        state.conversationHasOlderTurns =
            conversation.turn_page?.has_more === true;
        if (state.conversationHasOlderTurns) {
            elements.chatThread.append(createLoadOlderTurnsButton());
        }
        for (const turn of conversation.turns) {
            elements.chatThread.append(createTurnElement(turn));
        }
        setChatLayout(conversation.turns.length > 0, conversation.title);
        markActiveConversation();
        scrollToLatestTurn();
        const lastTurn = conversation.turns[conversation.turns.length - 1];
        state.latestTurnMaxUsagePercent = maxUsagePercentFromAgents(
            lastTurn?.agent_metrics
        );
        refreshContextIndicatorHeader();
    }

    function startNewConversation() {
        setMobileTab("chat");
        state.activeConversationId = null;
        state.activeConversationTitle = "";
        elements.chatThread.replaceChildren();
        elements.queryStatus.textContent = "";
        elements.useMemory.checked = preferredMemoryEnabled();
        updateMemoryStatus({enabled: elements.useMemory.checked});
        setChatLayout(false);
        markActiveConversation();
        state.latestTurnMaxUsagePercent = null;
        state.conversationNextBefore = 0;
        state.conversationHasOlderTurns = false;
        refreshContextIndicatorHeader();
        elements.question.focus();
    }

    return {
        createTurnElement,
        renderConversation,
        startNewConversation,
        streamQuery,
    };
}
