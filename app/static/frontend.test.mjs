import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import {fileURLToPath} from "node:url";

import {
    drainBuffer,
    readServerEvents,
    validateEventSequence,
} from "./sse.js";
import {APIError, errorFromResponse} from "./api.js";

const staticDir = path.dirname(fileURLToPath(import.meta.url));
const appDir = path.resolve(staticDir, "..");

test("parsea SSE fragmentado y conserva datos multilínea", () => {
    const first = drainBuffer(
        "event: retrieval\r\ndata: {\"count\":1}\r\n\r\nevent: del"
    );
    assert.deepEqual(first.events, [{name: "retrieval", data: {count: 1}}]);
    const second = drainBuffer(
        first.remainder + "ta\ndata: {\"text\":\"hola\"}\n\n"
    );
    assert.deepEqual(second.events, [{name: "delta", data: {text: "hola"}}]);
});

test("valida el contrato retrieval-stage-delta-done o error", () => {
    assert.equal(validateEventSequence([
        {name: "retrieval"}, {name: "stage"}, {name: "delta"},
        {name: "delta"}, {name: "done"},
    ]), true);
    assert.equal(validateEventSequence([
        {name: "retrieval"}, {name: "stage"}, {name: "error"},
    ]), true);
    assert.equal(validateEventSequence([{name: "error"}]), true);
    assert.equal(validateEventSequence([{name: "delta"}, {name: "done"}]), false);
    assert.equal(validateEventSequence([{name: "retrieval"}, {name: "done"}]), false);
    assert.equal(validateEventSequence([
        {name: "retrieval"}, {name: "delta"}, {name: "done"},
    ]), false);
});

test("parsea de extremo a extremo el contrato emitido por el servidor", () => {
    const wire = [
        "event: retrieval\ndata: {\"sources\":[],\"count\":0}\n\n",
        "event: stage\ndata: {\"stage\":\"writer\"}\n\n",
        "event: delta\ndata: {\"text\":\"respuesta\"}\n\n",
        "event: done\ndata: {\"answer\":\"respuesta\"}\n\n",
    ].join("");
    const parsed = drainBuffer(wire);
    assert.equal(parsed.remainder, "");
    assert.equal(validateEventSequence(parsed.events), true);
    assert.deepEqual(parsed.events.map((event) => event.name), [
        "retrieval", "stage", "delta", "done",
    ]);
});

test("cancela el lector si el consumidor abandona el flujo a mitad", async () => {
    const encoder = new TextEncoder();
    const chunks = [
        encoder.encode("event: retrieval\ndata: {\"count\":1}\n\n"),
        encoder.encode("event: stage\ndata: {\"stage\":\"writer\"}\n\n"),
    ];
    let index = 0;
    let cancelled = false;
    let released = false;
    const response = {
        body: {
            getReader: () => ({
                read: () => Promise.resolve(index < chunks.length
                    ? {done: false, value: chunks[index++]}
                    : {done: true, value: undefined}),
                cancel: () => {
                    cancelled = true;
                    return Promise.resolve();
                },
                releaseLock: () => {
                    released = true;
                },
            }),
        },
    };
    const seen = [];
    for await (const event of readServerEvents(response)) {
        seen.push(event.name);
        break;
    }
    assert.deepEqual(seen, ["retrieval"]);
    assert.equal(cancelled, true);
    assert.equal(released, true);
});

test("la entrada es un módulo ES y evita APIs de inyección HTML", () => {
    const html = fs.readFileSync(
        path.join(appDir, "templates", "index.html"), "utf8"
    );
    const app = fs.readFileSync(path.join(staticDir, "app.js"), "utf8");
    const chat = fs.readFileSync(path.join(staticDir, "chat.js"), "utf8");
    const modules = fs.readdirSync(staticDir)
        .filter((file) => file.endsWith(".js"))
        .map((file) => fs.readFileSync(path.join(staticDir, file), "utf8"))
        .join("\n");
    assert.match(html, /<script\s+type="module"\s+src="\/static\/app\.js/);
    assert.match(app, /from "\.\/api\.js"/);
    assert.match(app, /from "\.\/chat\.js"/);
    assert.match(chat, /from "\.\/sse\.js"/);
    assert.doesNotMatch(modules, /\.(?:innerHTML|outerHTML)\s*=/);
    assert.doesNotMatch(modules, /insertAdjacentHTML\s*\(/);
    assert.doesNotMatch(modules, /document\.write\s*\(/);
});

test("la entrada define las dependencias que entrega a los controladores", () => {
    const app = fs.readFileSync(path.join(staticDir, "app.js"), "utf8");
    for (const callback of [
        "preferredMemoryEnabled",
        "scrollToLatestTurn",
        "setChatLayout",
        "updateMemoryStatus",
    ]) {
        assert.match(app, new RegExp(`function\\s+${callback}\\s*\\(`));
    }
});

test("los errores API conservan el request_id visible para soporte", () => {
    const error = errorFromResponse({
        status: 500,
        headers: {get: (name) => name === "X-Request-ID" ? "abc123" : null},
    }, {detail: "No se pudo completar."});
    assert.equal(error instanceof APIError, true);
    assert.equal(error.requestId, "abc123");
    assert.match(error.message, /request_id: abc123/);
});
