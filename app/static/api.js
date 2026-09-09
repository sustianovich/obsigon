"use strict";

export class APIError extends Error {
        constructor(message, requestId, status) {
            const suffix = requestId ? ` (request_id: ${requestId})` : "";
            super(`${message}${suffix}`);
            this.name = "APIError";
            this.requestId = requestId || "";
            this.status = status || 0;
        }
    }

export function errorFromResponse(response, payload = {}) {
        const requestId = payload.request_id
            || response?.headers?.get?.("X-Request-ID")
            || "";
        return new APIError(
            payload.detail || "Se produjo un error inesperado.",
            requestId,
            response?.status
        );
    }

export async function request(url, options = {}) {
        const response = await fetch(url, {
            headers: {"Content-Type": "application/json"},
            ...options,
        });
        const payload = await response.json().catch(() => ({}));
        if (!response.ok) {
            throw errorFromResponse(response, payload);
        }
        return payload;
    }

if (typeof window !== "undefined") {
    window.ObsigonAPI = {APIError, errorFromResponse, request};
}
