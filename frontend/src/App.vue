<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from "vue";

interface PromptMessage {
  id: string;
  timestamp: string;
  prompt: string;
  tokens_in: number;
  tokens_out: number;
  water_cost_ml: number;
  status: string;
}

// --- Config ---
// 5.0 mL = 5,000 total tokens fills the 11,034m Mariana Trench from abyss to surface
const TRENCH_CAPACITY_ML = 5.0;
const MAX_DEPTH_M = 11034;

// --- State ---
const messages = ref<PromptMessage[]>([]);
const promptInput = ref("");
const isSubmitting = ref(false);
const wsConnected = ref(false);
const gyroEnabled = ref(false);
const needsGyroPermission = ref(false);

// Water physics state
let targetTilt = 0;
let currentTilt = 0;
let wavePhase = 0;
let animFrameId: number | null = null;
let ws: WebSocket | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let cleanupCanvas: (() => void) | null = null;

const canvasRef = ref<HTMLCanvasElement | null>(null);

// --- Totals & Trench Depth Metrics ---
const totalWaterMl = computed(() =>
  messages.value.reduce((sum, m) => sum + (Number(m.water_cost_ml) || 0), 0),
);

const totalTokens = computed(() =>
  messages.value.reduce(
    (sum, m) => sum + (Number(m.tokens_in) || 0) + (Number(m.tokens_out) || 0),
    0,
  ),
);

const fillRatio = computed(() =>
  Math.min(1, Math.max(0.03, totalWaterMl.value / TRENCH_CAPACITY_ML)),
);

const currentDepthMeters = computed(() =>
  Math.round(MAX_DEPTH_M * (1 - Math.min(1, totalWaterMl.value / TRENCH_CAPACITY_ML))),
);

// Bottom-up order: newest messages sit at the bottom right above the composer
const bottomUpMessages = computed(() => [...messages.value].reverse());

// --- API & Read-Only Upstream WebSocket ---
async function fetchHistory() {
  try {
    const res = await fetch("/api/history");
    if (!res.ok) return;
    const data = await res.json();
    messages.value = Array.isArray(data) ? data : [];
  } catch (err) {
    console.error("Failed to load history:", err);
  }
}

function connectWS() {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  ws = new WebSocket(`${proto}//${window.location.host}/ws`);

  ws.onopen = () => {
    wsConnected.value = true;
  };

  ws.onmessage = (event: MessageEvent) => {
    try {
      const msg = JSON.parse(event.data);
      handleWSMessage(msg);
    } catch (e) {
      console.error("Invalid WS payload:", e);
    }
  };

  ws.onclose = () => {
    wsConnected.value = false;
    reconnectTimer = setTimeout(connectWS, 2000);
  };

  ws.onerror = () => {
    ws?.close();
  };
}

function handleWSMessage(msg: any) {
  const idx = messages.value.findIndex((m) => m.id === msg.prompt_id);

  if (msg.type === "queued") {
    if (idx === -1) {
      messages.value.push({
        id: msg.prompt_id,
        timestamp: msg.timestamp,
        prompt: msg.prompt,
        tokens_in: 0,
        tokens_out: 0,
        water_cost_ml: 0,
        status: "pending",
      });
    }
  } else if (msg.type === "processing") {
    if (idx !== -1 && messages.value[idx]) {
      messages.value[idx].status = "processing";
    }
  } else if (msg.type === "completed") {
    if (idx !== -1 && messages.value[idx]) {
      messages.value[idx].status = "ollama_done";
      messages.value[idx].tokens_in = msg.tokens_in ?? 0;
      messages.value[idx].tokens_out = msg.tokens_out ?? 0;
      messages.value[idx].water_cost_ml = msg.water_cost_ml ?? 0;
    } else {
      messages.value.push({
        id: msg.prompt_id,
        timestamp: msg.timestamp,
        prompt: msg.prompt,
        tokens_in: msg.tokens_in ?? 0,
        tokens_out: msg.tokens_out ?? 0,
        water_cost_ml: msg.water_cost_ml ?? 0,
        status: "ollama_done",
      });
    }
  } else if (msg.type === "error") {
    if (idx !== -1 && messages.value[idx]) {
      messages.value[idx].status = "failed";
    }
  }
}

async function submitPrompt() {
  const text = promptInput.value.trim();
  if (!text || isSubmitting.value) return;

  if (needsGyroPermission.value) {
    requestGyroPermission();
  }

  isSubmitting.value = true;
  promptInput.value = "";

  try {
    const res = await fetch("/api/prompts", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt: text }),
    });
    if (!res.ok) {
      promptInput.value = text;
    }
  } catch (err) {
    console.error("Submit failed:", err);
    promptInput.value = text;
  } finally {
    isSubmitting.value = false;
  }
}

// --- Mobile Gyroscope & Desktop Mouse Parallax ---
function handleOrientation(e: DeviceOrientationEvent) {
  if (e.gamma === null) return;
  gyroEnabled.value = true;
  const clamped = Math.max(-45, Math.min(45, e.gamma));
  // Negative sign keeps water level with gravity: tilting right-down raises water on the right
  targetTilt = -(clamped / 45) * 0.28;
}

function handleMouseMove(e: MouseEvent) {
  if (gyroEnabled.value) return;
  const normX = (e.clientX / window.innerWidth) * 2 - 1;
  targetTilt = normX * 0.08;
}

async function requestGyroPermission() {
  const DOE = DeviceOrientationEvent as unknown as {
    requestPermission?: () => Promise<"granted" | "denied">;
  };
  if (typeof DOE !== "undefined" && typeof DOE.requestPermission === "function") {
    try {
      const res = await DOE.requestPermission();
      if (res === "granted") {
        window.addEventListener("deviceorientation", handleOrientation);
        needsGyroPermission.value = false;
        gyroEnabled.value = true;
      }
    } catch (e) {
      console.warn("Gyro permission denied:", e);
    }
  }
}

// --- Point-Cloud Mariana Trench & Rising Water Renderer ---
function trenchDepthAt(nx: number): number {
  const d = Math.abs(nx - 0.47);
  if (d < 0.035) {
    return 0.94 + (1 - d / 0.035) * 0.06;
  }
  if (d < 0.23) {
    const t = (d - 0.035) / 0.195;
    return 0.22 + Math.pow(1 - t, 1.35) * 0.72;
  }
  const shelfWave = Math.sin(nx * 19.0) * 0.025 + Math.cos(nx * 37.0) * 0.012;
  return 0.16 + shelfWave;
}

function startCanvas(): (() => void) | null {
  const canvas = canvasRef.value;
  if (!canvas) return null;
  const ctx = canvas.getContext("2d");
  if (!ctx) return null;

  let width = 0;
  let height = 0;
  let dpr = 1;

  const terrainCols = 140;
  const terrainRows = 46;
  const terrainPoints: Array<{
    nx: number;
    z: number;
    ridgeNoise: number;
    size: number;
    alpha: number;
  }> = [];

  for (let r = 0; r < terrainRows; r++) {
    const rowNorm = r / (terrainRows - 1);
    for (let c = 0; c < terrainCols; c++) {
      const colNorm = c / (terrainCols - 1);
      const seed = Math.sin(r * 127.1 + c * 311.7) * 43758.5453;
      const rand = seed - Math.floor(seed);
      const seed2 = Math.sin(r * 269.5 + c * 183.3) * 28461.712;
      const rand2 = seed2 - Math.floor(seed2);

      terrainPoints.push({
        nx: colNorm + (rand - 0.5) * 0.008,
        z: rowNorm,
        ridgeNoise:
          Math.sin(colNorm * 28 + rowNorm * 6) * 0.014 +
          Math.cos(colNorm * 65 - rowNorm * 9) * 0.007 +
          (rand2 - 0.5) * 0.012,
        size: (1 - rowNorm * 0.55) * (1.1 + rand * 1.1),
        alpha: (1 - rowNorm * 0.72) * (0.35 + rand2 * 0.65),
      });
    }
  }

  const waterMotes = Array.from({ length: 75 }, (_, i) => ({
    x: (i * 0.6180339) % 1,
    y: Math.random(),
    speed: 0.0004 + (i % 7) * 0.00012,
    size: 0.8 + (i % 3) * 0.6,
    phase: i * 1.3,
  }));

  function resize() {
    if (!canvas || !ctx) return;
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    width = window.innerWidth;
    height = window.innerHeight;
    canvas.width = width * dpr;
    canvas.height = height * dpr;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  }

  resize();
  window.addEventListener("resize", resize);

  let smoothedFill = fillRatio.value;

  function render() {
    if (!ctx) return;
    smoothedFill += (fillRatio.value - smoothedFill) * 0.04;
    currentTilt += (targetTilt - currentTilt) * 0.08;
    wavePhase += 0.025;

    const bgGrad = ctx.createRadialGradient(
      width * 0.5,
      height * 0.12,
      10,
      width * 0.5,
      height * 0.55,
      Math.max(width, height) * 0.85,
    );
    bgGrad.addColorStop(0, "#0c2342");
    bgGrad.addColorStop(0.55, "#061326");
    bgGrad.addColorStop(1, "#020813");
    ctx.fillStyle = bgGrad;
    ctx.fillRect(0, 0, width, height);

    const topMargin = height * 0.14;
    const bottomMargin = height * 0.93;
    const trenchSpanY = bottomMargin - topMargin;

    const baseWaterY = bottomMargin - smoothedFill * trenchSpanY;
    const waterYAt = (screenX: number) => {
      const relX = screenX - width * 0.5;
      const tiltOffset = Math.tan(currentTilt) * relX;
      const wave1 = Math.sin(screenX * 0.012 + wavePhase) * 4.5;
      const wave2 = Math.cos(screenX * 0.027 - wavePhase * 1.4) * 2.2;
      return baseWaterY + tiltOffset + wave1 + wave2;
    };

    // 1. Submerged water volume
    ctx.save();
    ctx.beginPath();
    ctx.moveTo(0, height);
    for (let x = 0; x <= width; x += 12) {
      ctx.lineTo(x, waterYAt(x));
    }
    ctx.lineTo(width, height);
    ctx.closePath();

    const waterGrad = ctx.createLinearGradient(0, baseWaterY - 30, 0, height);
    waterGrad.addColorStop(0, "rgba(34, 211, 238, 0.24)");
    waterGrad.addColorStop(0.25, "rgba(14, 116, 144, 0.18)");
    waterGrad.addColorStop(1, "rgba(3, 25, 48, 0.55)");
    ctx.fillStyle = waterGrad;
    ctx.fill();
    ctx.restore();

    // 2. Point-cloud Mariana Trench bathymetry
    for (let i = 0; i < terrainPoints.length; i++) {
      const p = terrainPoints[i];
      if (!p) continue;
      const perspectiveX = 0.5 + (p.nx - 0.5) * (1 - p.z * 0.14);
      const sx = perspectiveX * width;
      const depthNorm = trenchDepthAt(p.nx) + p.ridgeNoise;
      const sy = topMargin + depthNorm * trenchSpanY - p.z * (height * 0.065);

      if (sy > height + 4) continue;

      const wy = waterYAt(sx);
      if (sy >= wy) {
        ctx.fillStyle = `rgba(56, 232, 255, ${Math.min(0.95, p.alpha * 1.15)})`;
      } else {
        ctx.fillStyle = `rgba(96, 165, 250, ${p.alpha * 0.42})`;
      }

      ctx.fillRect(sx, sy, p.size, p.size);
    }

    // 3. Luminous waterline crest
    ctx.save();
    ctx.beginPath();
    for (let x = 0; x <= width; x += 4) {
      const y = waterYAt(x);
      if (x === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    }
    ctx.strokeStyle = "rgba(103, 232, 249, 0.75)";
    ctx.lineWidth = 1.6;
    ctx.shadowColor = "#22d3ee";
    ctx.shadowBlur = 12;
    ctx.stroke();

    for (let x = 6; x < width; x += 14) {
      const y = waterYAt(x) + Math.sin(x * 0.2 + wavePhase * 2) * 2;
      ctx.fillStyle = "rgba(186, 230, 253, 0.85)";
      ctx.fillRect(x, y - 1, 1.8, 1.8);
    }
    ctx.restore();

    // 4. Rising submerged motes
    for (const m of waterMotes) {
      m.y -= m.speed;
      if (m.y < 0) m.y = 1;
      const mx = m.x * width + Math.sin(wavePhase + m.phase) * 8;
      const wy = waterYAt(mx);
      const my = wy + m.y * Math.max(10, height - wy);
      if (my > wy + 4 && my < height) {
        ctx.fillStyle = "rgba(125, 211, 252, 0.35)";
        ctx.fillRect(mx, my, m.size, m.size);
      }
    }

    // 5. Left bathymetric depth scale ticks
    const ticks = [0, 2000, 4000, 6000, 8000, 10000, 11034];
    ctx.font = "10px ui-monospace, SFMono-Regular, Menlo, monospace";
    ctx.textAlign = "left";
    for (const depth of ticks) {
      const ty = topMargin + (depth / MAX_DEPTH_M) * trenchSpanY;
      ctx.fillStyle = "rgba(148, 163, 184, 0.45)";
      ctx.fillRect(14, ty, 10, 1);
      ctx.fillText(`${depth}m`, 28, ty + 3);
    }

    animFrameId = requestAnimationFrame(render);
  }

  animFrameId = requestAnimationFrame(render);

  return () => {
    window.removeEventListener("resize", resize);
    if (animFrameId) cancelAnimationFrame(animFrameId);
  };
}

onMounted(() => {
  fetchHistory();
  connectWS();
  cleanupCanvas = startCanvas();

  if (typeof window !== "undefined") {
    window.addEventListener("mousemove", handleMouseMove);
    const DOE = DeviceOrientationEvent as unknown as {
      requestPermission?: () => Promise<"granted" | "denied">;
    };
    if (typeof DOE !== "undefined" && typeof DOE.requestPermission === "function") {
      needsGyroPermission.value = true;
    } else if ("DeviceOrientationEvent" in window) {
      window.addEventListener("deviceorientation", handleOrientation);
    }
  }
});

onUnmounted(() => {
  if (cleanupCanvas) cleanupCanvas();
  if (reconnectTimer) clearTimeout(reconnectTimer);
  ws?.close();
  window.removeEventListener("mousemove", handleMouseMove);
  window.removeEventListener("deviceorientation", handleOrientation);
});

function cardClasses(status: string): string {
  switch (status) {
    case "pending":
      return "bg-amber-950/55 border-amber-400/45 text-amber-50";
    case "processing":
      return "bg-cyan-950/65 border-cyan-400/70 text-cyan-50 shadow-[0_0_20px_rgba(34,211,238,0.18)]";
    case "ollama_done":
    case "delivered":
      return "bg-emerald-950/55 border-emerald-400/45 text-emerald-50";
    case "failed":
      return "bg-red-950/60 border-red-400/50 text-red-100";
    default:
      return "bg-amber-950/55 border-amber-400/45 text-amber-50";
  }
}

function isCompleted(status: string): boolean {
  return status === "ollama_done" || status === "delivered";
}
</script>

<template>
  <div
    class="relative flex h-dvh w-screen flex-col overflow-hidden bg-[#020813] text-slate-200 antialiased"
  >
    <!-- Full-screen Point-Cloud Mariana Trench Canvas -->
    <canvas ref="canvasRef" class="pointer-events-none fixed inset-0 z-0 h-full w-full"></canvas>

    <!-- Top Bathymetric Telemetry Header -->
    <header
      class="relative z-10 flex items-center justify-between gap-3 bg-gradient-to-b from-[#020813]/90 to-transparent px-4 py-3.5 sm:px-6"
    >
      <div>
        <div class="flex items-center gap-2">
          <span
            class="h-2 w-2 rounded-full transition-all"
            :class="
              wsConnected
                ? 'bg-cyan-400 shadow-[0_0_10px_#22d3ee]'
                : 'bg-red-500 shadow-[0_0_8px_#ef4444]'
            "
          ></span>
          <h1 class="text-sm font-bold tracking-[0.16em] text-slate-50 sm:text-base">
            MARIANA TRENCH
          </h1>
        </div>
        <p class="mt-0.5 font-mono text-[11px] tracking-[0.14em] text-sky-300">
          AI WATER FOOTPRINT · DEPTH {{ currentDepthMeters.toLocaleString() }}m
        </p>
      </div>

      <div class="flex items-center gap-3">
        <div
          class="flex items-center gap-3 rounded-full border border-sky-400/30 bg-[#061428]/75 px-3.5 py-1.5 backdrop-blur-md"
        >
          <div class="flex flex-col items-end">
            <span class="text-[9px] tracking-[0.12em] text-slate-400">WATER USED</span>
            <span class="font-mono text-sm font-bold text-cyan-400">
              {{ totalWaterMl.toFixed(3) }} <span class="text-xs font-medium">mL</span>
            </span>
          </div>
          <div class="h-5 w-px bg-slate-400/25"></div>
          <div class="flex flex-col items-end">
            <span class="text-[9px] tracking-[0.12em] text-slate-400">TOKENS</span>
            <span class="font-mono text-sm font-bold text-slate-100">
              {{ totalTokens.toLocaleString() }}
            </span>
          </div>
        </div>

        <button
          v-if="needsGyroPermission"
          type="button"
          class="cursor-pointer rounded-lg border border-cyan-400/45 bg-cyan-700/35 px-2.5 py-1.5 text-xs font-medium text-cyan-100 hover:bg-cyan-700/55"
          @click="requestGyroPermission"
        >
          Enable Tilt
        </button>
      </div>
    </header>

    <!-- Bottom-Up Prompt & Water Cost Stream (4/5 Prompt | 1/5 Tokens & mL) -->
    <main
      class="relative z-5 flex flex-1 flex-col-reverse overflow-y-auto pt-3 pr-4 pb-2 pl-14 [mask-image:linear-gradient(to_bottom,transparent_0%,black_14%,black_100%)] [-webkit-mask-image:linear-gradient(to_bottom,transparent_0%,black_14%,black_100%)]"
    >
      <div class="mx-auto flex w-full max-w-2xl flex-col-reverse gap-2.5">
        <article
          v-for="item in bottomUpMessages"
          :key="item.id"
          class="flex items-center rounded-xl border backdrop-blur-md transition-colors duration-300"
          :class="cardClasses(item.status)"
        >
          <!-- 4/5 Width: Prompt Text -->
          <div class="w-4/5 px-3.5 py-2.5">
            <p class="break-words text-sm leading-snug">
              {{ item.prompt }}
            </p>
          </div>

          <!-- 1/5 Width: Tokens & Water Cost -->
          <div
            class="flex w-1/5 shrink-0 flex-col items-end justify-center self-stretch border-l border-white/10 px-2.5 py-2 text-right font-mono"
          >
            <template v-if="isCompleted(item.status)">
              <span class="text-xs font-bold text-emerald-300">
                +{{ Number(item.water_cost_ml || 0).toFixed(3) }}mL
              </span>
              <span class="mt-0.5 text-[11px] text-emerald-200/75">
                {{ (item.tokens_in || 0) + (item.tokens_out || 0) }} tok
              </span>
            </template>
            <template v-else>
              <span
                class="text-xs opacity-60"
                :class="{ 'animate-pulse text-cyan-300 opacity-100': item.status === 'processing' }"
              >
                — mL
              </span>
              <span class="mt-0.5 text-[11px] opacity-50">— tok</span>
            </template>
          </div>
        </article>
      </div>
    </main>

    <!-- Bottom Prompt Input Bar -->
    <footer
      class="relative z-10 bg-gradient-to-t from-[#020813]/95 via-[#020813]/75 to-transparent px-4 pt-2.5 pb-[calc(0.875rem+env(safe-area-inset-bottom))]"
    >
      <form class="mx-auto flex w-full max-w-2xl gap-2" @submit.prevent="submitPrompt">
        <input
          v-model="promptInput"
          type="text"
          placeholder="Send a prompt to measure its water cost..."
          :disabled="isSubmitting"
          autocomplete="off"
          class="flex-1 rounded-xl border border-sky-400/35 bg-[#07162b]/85 px-4 py-3 text-sm text-slate-50 placeholder-slate-500 backdrop-blur-md outline-none transition focus:border-cyan-400 focus:ring-2 focus:ring-cyan-400/20 disabled:opacity-50"
        />
        <button
          type="submit"
          :disabled="isSubmitting || !promptInput.trim()"
          class="cursor-pointer rounded-xl border border-cyan-400/55 bg-gradient-to-br from-cyan-600 to-sky-600 px-5 text-xs font-bold tracking-[0.1em] text-slate-50 transition hover:brightness-110 disabled:cursor-not-allowed disabled:opacity-40"
        >
          {{ isSubmitting ? "..." : "SEND" }}
        </button>
      </form>
    </footer>
  </div>
</template>
