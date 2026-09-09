"use strict";

export function parseEventBlock(block) {
        let name = "message";
        const dataLines = [];
        for (const line of block.replaceAll("\r\n", "\n").split("\n")) {
            if (line.startsWith("event:")) {
                name = line.slice(6).trim();
            } else if (line.startsWith("data:")) {
                dataLines.push(line.slice(5).trimStart());
            }
        }
        if (!dataLines.length) {
            return null;
        }
        return {name, data: JSON.parse(dataLines.join("\n"))};
    }

export function drainBuffer(buffer) {
        const events = [];
        let normalized = buffer.replaceAll("\r\n", "\n");
        let separator = normalized.indexOf("\n\n");
        while (separator !== -1) {
            const block = normalized.slice(0, separator);
            normalized = normalized.slice(separator + 2);
            const event = parseEventBlock(block);
            if (event) {
                events.push(event);
            }
            separator = normalized.indexOf("\n\n");
        }
        return {events, remainder: normalized};
    }

export async function* readServerEvents(response) {
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        try {
            while (true) {
                const {done, value} = await reader.read();
                buffer += decoder.decode(
                    value || new Uint8Array(), {stream: !done}
                );
                const drained = drainBuffer(buffer);
                buffer = drained.remainder;
                for (const event of drained.events) {
                    yield event;
                }
                if (done) {
                    break;
                }
            }
        } finally {
            // Si el consumidor abandona el bucle antes del final (un error al
            // pintar un evento, una cancelacion, el evento `error`...), la
            // respuesta quedaria a medio leer y el navegador conservaria la
            // conexion keep-alive en un estado inconsistente: la consulta
            // siguiente reutilizaria esa conexion y recibiria la cola de la
            // respuesta anterior ("responde a la pregunta anterior") o
            // acabaria agotando el cupo de conexiones ("network error").
            // Cancelar el lector cierra el flujo de forma limpia siempre.
            try {
                await reader.cancel();
            } catch (_) {
                // El flujo ya podia estar cerrado; cancelar es idempotente.
            }
            reader.releaseLock();
        }
    }

export function validateEventSequence(events) {
        const names = events.map((event) => event.name);
        if (names.length === 1 && names[0] === "error") {
            return true;
        }
        const terminal = names.at(-1);
        if (names[0] !== "retrieval" || (terminal !== "done" && terminal !== "error")) {
            return false;
        }
        let phase = "retrieval";
        let sawStage = false;
        for (const name of names.slice(1, -1)) {
            if (name === "stage" && phase !== "delta") {
                phase = "stage";
                sawStage = true;
            } else if (name === "delta" && sawStage) {
                phase = "delta";
            } else {
                return false;
            }
        }
        return sawStage;
    }

if (typeof window !== "undefined") {
    window.ObsigonSSE = {
        drainBuffer,
        parseEventBlock,
        readServerEvents,
        validateEventSequence,
    };
}
