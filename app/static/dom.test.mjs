import assert from "node:assert/strict";
import test from "node:test";

import {parseHTML} from "linkedom";

import {createChatController} from "./chat.js";
import {createContextInspector} from "./context-inspector.js";
import {createProjectsController} from "./projects.js";
import {createStore, state} from "./state.js";

function installDOM(body = "") {
    const {window} = parseHTML(`<!doctype html><html><body>${body}</body></html>`);
    Object.defineProperties(globalThis, {
        document: {value: window.document, configurable: true},
        navigator: {value: window.navigator, configurable: true},
        window: {value: window, configurable: true},
    });
    const values = new Map();
    Object.defineProperty(window, "localStorage", {
        configurable: true,
        value: {
            getItem: (key) => values.get(key) ?? null,
            removeItem: (key) => values.delete(key),
            setItem: (key, value) => values.set(key, String(value)),
        },
    });
    return window;
}

test("el store observable notifica cambios reales y permite desuscribirse", () => {
    const store = createStore({count: 0});
    const changes = [];
    const unsubscribe = store.subscribe((change) => changes.push(change));

    store.state.count = 1;
    store.state.count = 1;
    store.patch({count: 2, ready: true});
    unsubscribe();
    store.state.count = 3;

    assert.deepEqual(
        changes.map(({property, previous, value}) => ({
            property,
            previous,
            value,
        })),
        [
            {property: "count", previous: 0, value: 1},
            {property: "count", previous: 1, value: 2},
            {property: "ready", previous: undefined, value: true},
        ]
    );
    assert.deepEqual(store.snapshot(), {count: 3, ready: true});
});

test("el chat renderiza citas seguras y abre la fuente correspondiente", () => {
    installDOM('<section id="thread"></section>');
    const {createTurnElement} = createChatController({
        elements: {chatThread: document.getElementById("thread")},
        createAgentTrace: () => null,
        createRetrievalTrace: () => null,
        markActiveConversation: () => {},
        preferredMemoryEnabled: () => true,
        refreshContextIndicatorHeader: () => {},
        scrollToLatestTurn: () => {},
        setChatLayout: () => {},
        setMobileTab: () => {},
        updateMemoryStatus: () => {},
    });
    const turn = createTurnElement({
        id: "turn/1",
        question: "¿Qué dice la norma?",
        answer: "Texto <img src=x onerror=alert(1)> respaldado por [1].",
        sources: [
            {
                reference: "[1]",
                title: "Norma interna",
                path: "Normativa/Norma.md",
                heading: "Aplicación",
                reason: "Coincidencia semántica",
                content: "Contenido verificable",
                score: 0.91,
                tags: ["legal"],
                metadata: {estado: "vigente"},
                obsidian_url: "obsidian://open?vault=Prueba&file=Norma",
            },
        ],
    });
    document.getElementById("thread").append(turn);

    const source = turn.querySelector(".source-card");
    let scrolled = false;
    source.scrollIntoView = () => {
        scrolled = true;
    };
    turn.querySelector(".citation").click();

    assert.equal(turn.querySelector("img"), null);
    assert.match(turn.querySelector(".answer-card").textContent, /<img/);
    assert.equal(source.open, true);
    assert.equal(source.closest(".turn-sources").open, true);
    assert.equal(source.classList.contains("source-highlight"), true);
    assert.equal(scrolled, true);
    assert.equal(
        turn.querySelector(".obsidian-link").getAttribute("href"),
        "obsidian://open?vault=Prueba&file=Norma"
    );
});

test("una pregunta SSE sustituye el turno provisional por la respuesta final", async () => {
    installDOM('<section id="thread"></section><p id="status"></p>');
    const source = {
        reference: "[1]",
        title: "Procedimiento",
        path: "Procesos/Alta.md",
        heading: "Pasos",
        reason: "Coincidencia híbrida",
        content: "Paso verificado",
        score: 0.88,
        metadata: {estado: "vigente"},
    };
    const events = [
        `event: retrieval\ndata: ${JSON.stringify({sources: [source]})}\n\n`,
        'event: stage\ndata: {"stage":"writer"}\n\n',
        'event: delta\ndata: {"text":"Respuesta "}\n\n',
        'event: delta\ndata: {"text":"verificada [1]."}\n\n',
        `event: done\ndata: ${JSON.stringify({
            turn_id: "turn-sse",
            conversation_id: "conversation-sse",
            answer: "Respuesta verificada [1].",
            chat_model: "modelo-local",
            agents: {},
        })}\n\n`,
    ].join("");
    let request = null;
    globalThis.fetch = async (url, options) => {
        request = {url, options};
        return new Response(events, {
            headers: {"Content-Type": "text/event-stream"},
        });
    };
    const elements = {
        chatThread: document.getElementById("thread"),
        queryStatus: document.getElementById("status"),
    };
    const {streamQuery} = createChatController({
        elements,
        createAgentTrace: () => null,
        createRetrievalTrace: () => null,
        markActiveConversation: () => {},
        preferredMemoryEnabled: () => true,
        refreshContextIndicatorHeader: () => {},
        scrollToLatestTurn: () => {},
        setChatLayout: () => {},
        setMobileTab: () => {},
        updateMemoryStatus: () => {},
    });

    const result = await streamQuery(
        "¿Cuál es el procedimiento?",
        {question: "¿Cuál es el procedimiento?"},
        new AbortController().signal
    );

    assert.equal(request.url, "/api/query/stream");
    assert.equal(request.options.method, "POST");
    assert.equal(result.conversation_id, "conversation-sse");
    assert.equal(elements.chatThread.children.length, 1);
    assert.equal(elements.chatThread.querySelector(".streaming-answer"), null);
    assert.equal(
        elements.chatThread.querySelector(".answer-card").textContent,
        "Respuesta verificada [1]."
    );
    assert.equal(elements.chatThread.querySelectorAll(".citation").length, 1);
    assert.equal(elements.chatThread.querySelectorAll(".source-card").length, 1);
});

test("el inspector de contexto presenta modelo, ventanas y consumo", () => {
    installDOM('<section id="role-panel"></section>');
    const {renderContextRolePanel} = createContextInspector({
        elements: {},
        supportsDialog: false,
    });
    const panel = document.getElementById("role-panel");

    renderContextRolePanel(panel, {
        status: "completed",
        model: "qwen-local",
        configured_context_window: 8192,
        effective_context_window: 4096,
        prompt_tokens: 1000,
        completion_tokens: 250,
        total_tokens: 1250,
        remaining_tokens: 2846,
        usage_percent: 30.5,
        estimated: false,
    });

    assert.match(panel.textContent, /Activado/);
    assert.match(panel.textContent, /qwen-local/);
    assert.match(panel.textContent, /8K/);
    assert.match(panel.textContent, /4K/);
    assert.equal(
        panel.querySelector(".context-meter").getAttribute("aria-label"),
        "Uso de contexto: 30.5%"
    );
});

test("proyectos e historial conservan selección y abren la conversación", async () => {
    const window = installDOM(`
        <div id="projects"></div>
        <p id="projects-empty"></p>
        <div id="conversations"></div>
        <p id="conversations-empty"></p>
        <button id="load-more"></button>
        <details id="history"><summary>Historial</summary></details>
        <p id="query-status"></p>
        <h2 id="conversation-title"></h2>
    `);
    const calls = [];
    globalThis.fetch = async (url) => {
        calls.push(String(url));
        if (url === "/api/projects") {
            return Response.json({
                default_project_id: "general",
                projects: [
                    {
                        id: "general",
                        name: "General",
                        conversation_count: 0,
                        context_usage: {},
                    },
                    {
                        id: "legal",
                        name: "Legal",
                        conversation_count: 1,
                        context_usage: {latest_percent: 24.5},
                    },
                ],
            });
        }
        if (String(url).startsWith("/api/conversations?project_id=legal")) {
            return Response.json({
                conversations: [
                    {id: "conv-1", title: "Consulta legal", turn_count: 2},
                ],
                pagination: {total: 1, has_more: false, next_offset: 1},
            });
        }
        if (url === "/api/conversations/conv-1") {
            return Response.json({
                id: "conv-1",
                title: "Consulta legal",
                turns: [],
            });
        }
        return Response.json({detail: "Ruta inesperada"}, {status: 404});
    };

    state.activeProjectId = null;
    state.activeProjectName = "";
    state.activeConversationId = null;
    state.defaultProjectId = "default";
    const renderedConversations = [];
    const elements = {
        conversationList: document.getElementById("conversations"),
        conversationListEmpty: document.getElementById("conversations-empty"),
        conversationLoadMore: document.getElementById("load-more"),
        conversationTitle: document.getElementById("conversation-title"),
        historyDrawer: document.getElementById("history"),
        projectList: document.getElementById("projects"),
        projectListEmpty: document.getElementById("projects-empty"),
        queryStatus: document.getElementById("query-status"),
    };
    const controller = createProjectsController({
        askForConfirmation: async () => true,
        askForText: async () => null,
        elements,
        markActiveConversation: () => {},
        markActiveProject: () => {},
        refreshContextIndicatorHeader: () => {},
        renderConversation: (conversation) => {
            renderedConversations.push(conversation);
        },
        renderProjectAgentSettings: () => {},
        setMobileTab: () => {},
        startNewConversation: () => {
            state.activeConversationId = null;
        },
        updateActiveProjectIndicators: () => {},
    });

    await controller.loadProjects({loadThreads: false});
    assert.equal(elements.projectList.children.length, 2);
    assert.equal(state.activeProjectId, "general");
    assert.equal(window.localStorage.getItem("obsidian-rag-active-project"), "general");

    await controller.selectProject("legal", "Legal");
    assert.equal(state.activeProjectId, "legal");
    assert.equal(elements.conversationList.children.length, 1);
    assert.equal(elements.historyDrawer.querySelector("summary").textContent, "Historial (1)");
    assert.deepEqual(renderedConversations.map(({id}) => id), ["conv-1"]);
    assert.deepEqual(calls, [
        "/api/projects",
        "/api/conversations?project_id=legal&limit=50&offset=0",
        "/api/conversations/conv-1",
    ]);
});
