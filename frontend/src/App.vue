<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, nextTick } from "vue";

type PromptStatus = "pending" | "processing" | "ollama_done" | "delivered" | "failed";

interface ChatItem {
  id: string;
  timestamp: string;
  prompt: string;
  response: string;
  tokens_in: number;
  tokens_out: number;
  status: PromptStatus;
}

interface WSMessage {
  type: "queued" | "processing" | "completed" | "error";
  prompt_id?: string;
  prompt?: string;
  response?: string;
  tokens_in?: number;
  tokens_out?: number;
  timestamp?: string;
  error?: string;
}

const messages = ref<ChatItem[]>([]);
const inputPrompt = ref("");
const wsConnected = ref(false);
const chatContainer = ref<HTMLElement | null>(null);

let socket: WebSocket | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;

const totalTokens = computed(() =>
  messages.value.reduce(
    (acc, m) => {
      acc.in += m.tokens_in || 0;
      acc.out += m.tokens_out || 0;
      return acc;
    },
    { in: 0, out: 0 },
  ),
);

const scrollToBottom = async () => {
  await nextTick();
  if (chatContainer.value) {
    chatContainer.value.scrollTop = chatContainer.value.scrollHeight;
  }
};

async function fetchHistory(): Promise<void> {
  try {
    const res = await fetch("/api/history");
    if (res.ok) {
      const data: ChatItem[] = await res.json();
      messages.value = data;
      await scrollToBottom();
    }
  } catch (err) {
    console.error("Failed to fetch history:", err);
  }
}

function connectWS(): void {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  const wsUrl = `${protocol}//${window.location.host}/ws`;

  socket = new WebSocket(wsUrl);

  socket.onopen = () => {
    wsConnected.value = true;
  };

  socket.onmessage = async (event: MessageEvent<string>) => {
    const msg: WSMessage = JSON.parse(event.data);

    if (msg.type === "queued" && msg.prompt_id) {
      const exists = messages.value.some((m) => m.id === msg.prompt_id);
      if (!exists) {
        messages.value.push({
          id: msg.prompt_id,
          prompt: msg.prompt ?? "",
          response: "",
          tokens_in: 0,
          tokens_out: 0,
          status: "pending",
          timestamp: msg.timestamp ?? new Date().toISOString(),
        });
        await scrollToBottom();
      }
    } else if (msg.type === "processing" && msg.prompt_id) {
      const item = messages.value.find((m) => m.id === msg.prompt_id);
      if (item) item.status = "processing";
    } else if (msg.type === "completed" && msg.prompt_id) {
      const item = messages.value.find((m) => m.id === msg.prompt_id);
      if (item) {
        item.response = msg.response ?? "";
        item.tokens_in = msg.tokens_in ?? 0;
        item.tokens_out = msg.tokens_out ?? 0;
        item.status = "ollama_done";
        await scrollToBottom();
      }
    } else if (msg.type === "error" && msg.prompt_id) {
      const item = messages.value.find((m) => m.id === msg.prompt_id);
      if (item) {
        item.status = "failed";
        item.response = `Error: ${msg.error ?? "Unknown error"}`;
      }
    }
  };

  socket.onclose = () => {
    wsConnected.value = false;
    reconnectTimer = setTimeout(connectWS, 2000);
  };

  socket.onerror = () => {
    socket?.close();
  };
}

function sendPrompt(): void {
  const text = inputPrompt.value.trim();
  if (!text || !wsConnected.value || !socket) return;

  socket.send(JSON.stringify({ prompt: text }));
  inputPrompt.value = "";
}

function handleKeydown(e: KeyboardEvent): void {
  if (e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    sendPrompt();
  }
}

function statusMeta(status: PromptStatus): { label: string; classes: string } {
  switch (status) {
    case "pending":
      return { label: "Queued", classes: "bg-amber-500/15 text-amber-400" };
    case "processing":
      return { label: "Generating...", classes: "bg-blue-500/15 text-blue-400" };
    case "ollama_done":
    case "delivered":
      return { label: "Done", classes: "bg-emerald-500/15 text-emerald-400" };
    case "failed":
      return { label: "Failed", classes: "bg-red-500/15 text-red-400" };
  }
}

onMounted(() => {
  fetchHistory();
  connectWS();
});

onUnmounted(() => {
  if (reconnectTimer) clearTimeout(reconnectTimer);
  socket?.close();
});
</script>

<template>
  <div class="flex h-dvh flex-col bg-zinc-950 text-zinc-100 antialiased">
    <header class="border-b border-zinc-800/80 bg-zinc-950/90 backdrop-blur">
      <div class="mx-auto flex max-w-4xl items-center justify-between px-4 py-3.5">
        <div class="flex items-center gap-2.5">
          <span
            class="h-2.5 w-2.5 rounded-full transition-colors"
            :class="wsConnected ? 'bg-emerald-500 shadow-[0_0_8px_#10b981]' : 'bg-red-500'"
          />
          <h1 class="text-base font-semibold tracking-tight">Hackathon AI</h1>
        </div>

        <div class="flex items-center gap-2 text-xs text-zinc-400 tabular-nums">
          <span class="rounded-full border border-zinc-800 bg-zinc-900 px-3 py-1">
            Tokens In: <strong class="font-semibold text-zinc-100">{{ totalTokens.in }}</strong>
          </span>
          <span class="rounded-full border border-zinc-800 bg-zinc-900 px-3 py-1">
            Tokens Out: <strong class="font-semibold text-zinc-100">{{ totalTokens.out }}</strong>
          </span>
        </div>
      </div>
    </header>

    <!-- Chat Stream -->
    <main ref="chatContainer" class="flex-1 overflow-y-auto">
      <div class="mx-auto flex min-h-full max-w-4xl flex-col gap-6 px-4 py-6">
        <div v-if="messages.length === 0" class="my-auto mx-auto max-w-md text-center">
          <h2 class="mb-2 text-lg font-semibold text-zinc-100">
            Ask anything to drive the installation
          </h2>
          <p class="text-sm leading-relaxed text-zinc-400">
            Prompts are queued sequentially, processed via Ollama, and streamed as token telemetry
            to TouchDesigner.
          </p>
        </div>

        <div v-for="item in messages" :key="item.id" class="flex flex-col gap-3">
          <!-- User Bubble -->
          <div class="flex justify-end">
            <div
              class="max-w-[78%] whitespace-pre-wrap rounded-2xl border border-zinc-700/70 bg-zinc-800/90 px-4 py-3 text-sm leading-relaxed text-zinc-100"
            >
              {{ item.prompt }}
            </div>
          </div>

          <!-- Assistant Bubble -->
          <div class="flex items-start gap-3">
            <div
              class="mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-blue-600 text-xs font-bold text-white"
            >
              AI
            </div>

            <div
              class="w-full rounded-2xl border border-zinc-800 bg-zinc-900/90 px-4 py-3 text-sm leading-relaxed"
            >
              <div class="mb-2 flex items-center justify-between text-xs">
                <span
                  class="rounded px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wider"
                  :class="statusMeta(item.status).classes"
                >
                  {{ statusMeta(item.status).label }}
                </span>

                <span v-if="item.tokens_in || item.tokens_out" class="text-zinc-400 tabular-nums">
                  {{ item.tokens_in }} in · {{ item.tokens_out }} out
                </span>
              </div>

              <div v-if="item.response" class="whitespace-pre-wrap text-zinc-100">
                {{ item.response }}
              </div>

              <div
                v-else-if="item.status === 'processing'"
                class="flex items-center gap-1.5 py-1.5"
              >
                <span
                  class="h-2 w-2 animate-bounce rounded-full bg-zinc-400 [animation-delay:-0.3s]"
                />
                <span
                  class="h-2 w-2 animate-bounce rounded-full bg-zinc-400 [animation-delay:-0.15s]"
                />
                <span class="h-2 w-2 animate-bounce rounded-full bg-zinc-400" />
              </div>

              <div v-else class="italic text-zinc-500">Waiting in queue...</div>
            </div>
          </div>
        </div>
      </div>
    </main>

    <!-- Prompt Composer -->
    <footer class="border-t border-zinc-800/80 bg-zinc-950 px-4 py-4">
      <form
        class="mx-auto flex max-w-4xl items-end gap-2.5 rounded-2xl border border-zinc-800 bg-zinc-900 p-2 focus-within:border-zinc-700"
        @submit.prevent="sendPrompt"
      >
        <textarea
          v-model="inputPrompt"
          rows="1"
          placeholder="Send a prompt to the queue... (Enter to send, Shift+Enter for newline)"
          class="max-h-36 flex-1 resize-none bg-transparent px-2.5 py-1.5 text-sm text-zinc-100 placeholder-zinc-500 focus:outline-none"
          @keydown="handleKeydown"
        />
        <button
          type="submit"
          :disabled="!wsConnected || !inputPrompt.trim()"
          class="rounded-xl bg-blue-600 px-4 py-2 text-sm font-semibold text-white transition hover:bg-blue-500 disabled:cursor-not-allowed disabled:opacity-40"
        >
          Send
        </button>
      </form>
    </footer>
  </div>
</template>
