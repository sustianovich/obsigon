"use strict";

export const STORAGE_KEYS = Object.freeze({
    activeProject: "obsidian-rag-active-project",
    memoryPreference: "obsidian-rag-use-memory",
    theme: "obsidian-rag-theme",
    sidebarAdvanced: "obsidian-rag-sidebar-advanced",
});

export function createStore(initialState = {}) {
    const listeners = new Set();
    const state = new Proxy({...initialState}, {
        set(target, property, value) {
            const previous = target[property];
            if (Object.is(previous, value)) {
                return true;
            }
            target[property] = value;
            for (const listener of listeners) {
                listener({property, value, previous, state});
            }
            return true;
        },
    });
    return Object.freeze({
        state,
        patch(values) {
            for (const [property, value] of Object.entries(values)) {
                state[property] = value;
            }
        },
        subscribe(listener) {
            listeners.add(listener);
            return () => listeners.delete(listener);
        },
        snapshot() {
            return {...state};
        },
    });
}

export const appStore = createStore({
    activeConversationId: null,
    activeConversationTitle: "",
    activeProjectId: null,
    activeProjectName: "",
    defaultProjectId: "default",
    loadedProjects: [],
    pendingTurnNumber: 0,
    activeQueryController: null,
    memorySummaryInterval: 10,
    memoryRecentTurns: 4,
    lastStatusData: null,
    latestTurnMaxUsagePercent: null,
    conversationOffset: 0,
    conversationHasMore: false,
    conversationNextBefore: 0,
    conversationHasOlderTurns: false,
    indexingActive: false,
    streamingSupported: true,
    nativeVaultPickerAvailable: false,
});

export const state = appStore.state;
