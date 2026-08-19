import { useCallback, useEffect, useRef, useState } from "react";

// Architecture map: a force-directed canvas over GET /api/v1/topology.
// The layout model follows the logdoc.org topology demo: soft repulsion,
// edge springs, damping, then the simulation goes to sleep.

type ApiNode = {
  app: string;
  first_seen: string;
  last_seen: string;
  count: number;
  errors: number;
};

type ApiEdge = {
  src: string;
  dst: string;
  origin: string;
  first_seen: string;
  last_seen: string;
  count: number;
  errors: number;
  rps: number;
  error_rate: number;
};

type SimNode = ApiNode & {
  x: number;
  y: number;
  vx: number;
  vy: number;
  fixed: boolean;
  degree: number;
};

type Selection = { kind: "node"; app: string } | { kind: "edge"; src: string; dst: string } | null;

const ACCENT = "#e35b28";
const BAD = "#ff4f4f"; // alarm red, deliberately far from the accent orange
const BAD_RATE = 0.05; // error rate above which a node/edge is drawn as failing
const WINDOWS = ["5m", "15m", "1h", "24h"];

function apiKeyParam(): string {
  const key = localStorage.getItem("logdoc_api_key");
  return key ? `&api_key=${encodeURIComponent(key)}` : "";
}

function nodeRadius(n: SimNode): number {
  return Math.min(7 + Math.sqrt(n.degree) * 0.8 + Math.log10(1 + n.count) * 0.6, 16);
}

export default function Topology({ onOpenLogs }: { onOpenLogs: (app: string, tail: boolean) => void }) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const nodesRef = useRef<Map<string, SimNode>>(new Map());
  const edgesRef = useRef<ApiEdge[]>([]);
  const viewRef = useRef({ ox: 0, oy: 0, zm: 1 });
  const hotRef = useRef(400); // remaining simulation ticks before sleep
  const hoverRef = useRef<string | null>(null);
  const selectionRef = useRef<Selection>(null);
  const dragRef = useRef<{ mode: "pan" | "node"; app?: string; sx: number; sy: number } | null>(null);

  const [selection, setSelection] = useState<Selection>(null);
  const [win, setWin] = useState("5m");
  const [error, setError] = useState<string | null>(null);
  const [empty, setEmpty] = useState(false);
  // Bump to re-render the panel when polled data changes.
  const [, setDataTick] = useState(0);

  selectionRef.current = selection;

  const load = useCallback(async () => {
    try {
      const res = await fetch(`/api/v1/topology?window=${win}${apiKeyParam()}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: { nodes: ApiNode[]; edges: ApiEdge[] } = await res.json();
      const prev = nodesRef.current;
      const next = new Map<string, SimNode>();
      const degree = new Map<string, number>();
      for (const e of data.edges) {
        degree.set(e.src, (degree.get(e.src) ?? 0) + 1);
        degree.set(e.dst, (degree.get(e.dst) ?? 0) + 1);
      }
      const canvas = canvasRef.current;
      const w = canvas ? canvas.clientWidth : 800;
      const h = canvas ? canvas.clientHeight : 500;
      data.nodes.forEach((n, i) => {
        const old = prev.get(n.app);
        const angle = (i / Math.max(data.nodes.length, 1)) * Math.PI * 2;
        next.set(n.app, {
          ...n,
          degree: degree.get(n.app) ?? 0,
          x: old ? old.x : w / 2 + Math.cos(angle) * 120 + Math.random() * 20,
          y: old ? old.y : h / 2 + Math.sin(angle) * 120 + Math.random() * 20,
          vx: old ? old.vx : 0,
          vy: old ? old.vy : 0,
          fixed: old ? old.fixed : false,
        });
      });
      nodesRef.current = next;
      edgesRef.current = data.edges;
      setEmpty(data.nodes.length === 0);
      setError(null);
      hotRef.current = Math.max(hotRef.current, prev.size === data.nodes.length ? 60 : 400);
      setDataTick((t) => t + 1);
    } catch (e) {
      setError(String(e));
    }
  }, [win]);

  useEffect(() => {
    load();
    const iv = setInterval(load, 10000);
    return () => clearInterval(iv);
  }, [load]);

  // Simulation + rendering loop.
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    let raf = 0;

    const tick = () => {
      const w = canvas.clientWidth;
      const h = canvas.clientHeight;
      const dpr = window.devicePixelRatio || 1;
      if (canvas.width !== w * dpr || canvas.height !== h * dpr) {
        canvas.width = w * dpr;
        canvas.height = h * dpr;
      }

      const nodes = [...nodesRef.current.values()];
      const edges = edgesRef.current;

      if (hotRef.current > 0) {
        hotRef.current--;
        // Repulsion between every pair (capped range).
        for (let i = 0; i < nodes.length; i++) {
          for (let j = i + 1; j < nodes.length; j++) {
            const a = nodes[i];
            const b = nodes[j];
            const dx = b.x - a.x;
            const dy = b.y - a.y;
            const d = Math.hypot(dx, dy) || 1;
            if (d > 250) continue;
            const f = Math.min(400 / (d * d), 1.2);
            const fx = (dx / d) * f;
            const fy = (dy / d) * f;
            a.vx -= fx;
            a.vy -= fy;
            b.vx += fx;
            b.vy += fy;
          }
        }
        // Edge springs, rest length 100.
        for (const e of edges) {
          const a = nodesRef.current.get(e.src);
          const b = nodesRef.current.get(e.dst);
          if (!a || !b) continue;
          const dx = b.x - a.x;
          const dy = b.y - a.y;
          const d = Math.hypot(dx, dy) || 1;
          const f = (d - 100) * 0.008; // spring toward rest length 100
          a.vx += (dx / d) * f;
          a.vy += (dy / d) * f;
          b.vx -= (dx / d) * f;
          b.vy -= (dy / d) * f;
        }
        // Gentle pull to the center so disconnected parts stay in view.
        for (const n of nodes) {
          n.vx += (w / 2 - n.x) * 0.0005;
          n.vy += (h / 2 - n.y) * 0.0005;
        }
        for (const n of nodes) {
          if (n.fixed) {
            n.vx = 0;
            n.vy = 0;
            continue;
          }
          n.vx *= 0.82;
          n.vy *= 0.82;
          n.x += n.vx;
          n.y += n.vy;
        }
      }

      // --- draw ---
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, w, h);
      const { ox, oy, zm } = viewRef.current;
      ctx.translate(w / 2, h / 2);
      ctx.scale(zm, zm);
      ctx.translate(-w / 2 + ox, -h / 2 + oy);

      const now = performance.now();
      const sel = selectionRef.current;
      const hover = hoverRef.current;
      const focusApp = sel?.kind === "node" ? sel.app : hover;
      const neighbors = new Set<string>();
      if (focusApp) {
        neighbors.add(focusApp);
        for (const e of edges) {
          if (e.src === focusApp) neighbors.add(e.dst);
          if (e.dst === focusApp) neighbors.add(e.src);
        }
      }

      for (const e of edges) {
        const a = nodesRef.current.get(e.src);
        const b = nodesRef.current.get(e.dst);
        if (!a || !b) continue;
        const isSel = sel?.kind === "edge" && sel.src === e.src && sel.dst === e.dst;
        const lit = isSel || (focusApp !== null && (e.src === focusApp || e.dst === focusApp));
        const dim = (focusApp !== null || sel?.kind === "edge") && !lit;
        const bad = e.error_rate > BAD_RATE;
        ctx.strokeStyle = dim
          ? bad
            ? "rgba(255,79,79,0.2)"
            : "rgba(120,126,140,0.12)"
          : isSel
            ? ACCENT
            : bad
              ? BAD
              : lit
                ? "rgba(227,91,40,0.8)"
                : "rgba(120,126,140,0.45)";
        ctx.lineWidth = (isSel ? 2.2 : bad ? 2 : lit ? 1.8 : 1.1) / zm;
        // Failing edges: marching red dashes, so the failure reads as live.
        if (bad && !dim) {
          ctx.setLineDash([7 / zm, 5 / zm]);
          ctx.lineDashOffset = -(now / 40) / zm;
        }
        ctx.beginPath();
        ctx.moveTo(a.x, a.y);
        ctx.lineTo(b.x, b.y);
        ctx.stroke();
        ctx.setLineDash([]);

        // Direction arrow at the destination edge of the line.
        const dx = b.x - a.x;
        const dy = b.y - a.y;
        const d = Math.hypot(dx, dy) || 1;
        const rB = nodeRadius(b);
        const tipX = b.x - (dx / d) * (rB + 3);
        const tipY = b.y - (dy / d) * (rB + 3);
        const ah = 6 / zm + 2;
        const ang = Math.atan2(dy, dx);
        ctx.fillStyle = ctx.strokeStyle;
        ctx.beginPath();
        ctx.moveTo(tipX, tipY);
        ctx.lineTo(tipX - ah * Math.cos(ang - 0.4), tipY - ah * Math.sin(ang - 0.4));
        ctx.lineTo(tipX - ah * Math.cos(ang + 0.4), tipY - ah * Math.sin(ang + 0.4));
        ctx.closePath();
        ctx.fill();

        // Rate label on lit edges — and always on failing ones.
        if ((lit || isSel || (bad && !dim)) && zm > 0.5 && e.rps > 0) {
          ctx.fillStyle = bad ? "rgba(255,122,110,0.95)" : "rgba(216,219,226,0.85)";
          ctx.font = `${11 / zm}px ui-monospace, monospace`;
          const label =
            e.error_rate > 0
              ? `${e.rps.toFixed(e.rps < 10 ? 1 : 0)} rps · ${(e.error_rate * 100).toFixed(1)}% err`
              : `${e.rps.toFixed(e.rps < 10 ? 1 : 0)} rps`;
          ctx.fillText(label, (a.x + b.x) / 2 + 6 / zm, (a.y + b.y) / 2 - 6 / zm);
        }
      }

      for (const n of nodes) {
        const r = nodeRadius(n);
        const isSel = sel?.kind === "node" && sel.app === n.app;
        const inEdgeSel = sel?.kind === "edge" && (sel.src === n.app || sel.dst === n.app);
        const lit = focusApp === null ? sel?.kind !== "edge" || inEdgeSel : neighbors.has(n.app);
        const errRate = n.count > 0 ? n.errors / n.count : 0;
        const bad = errRate > BAD_RATE;
        ctx.globalAlpha = lit ? 1 : 0.25;
        // Failing node: pulsing red halo, unmistakable even at a glance.
        if (bad) {
          const pulse = (Math.sin(now / 260) + 1) / 2; // 0..1
          ctx.strokeStyle = `rgba(255,79,79,${0.55 - 0.35 * pulse})`;
          ctx.lineWidth = 2 / zm;
          ctx.beginPath();
          ctx.arc(n.x, n.y, r + (3 + pulse * 5) / zm, 0, Math.PI * 2);
          ctx.stroke();
        }
        ctx.fillStyle = bad ? BAD : ACCENT;
        ctx.beginPath();
        ctx.arc(n.x, n.y, r, 0, Math.PI * 2);
        ctx.fill();
        if (isSel || hover === n.app) {
          ctx.strokeStyle = "#fff";
          ctx.lineWidth = 1.5 / zm;
          ctx.stroke();
        }
        ctx.fillStyle = lit ? "#d8dbe2" : "rgba(216,219,226,0.5)";
        ctx.font = `${12 / Math.max(zm, 0.8)}px ui-monospace, monospace`;
        ctx.textAlign = "center";
        ctx.fillText(n.app, n.x, n.y + r + 14 / Math.max(zm, 0.8));
        if (bad) {
          ctx.fillStyle = lit ? "rgba(255,122,110,0.95)" : "rgba(255,122,110,0.4)";
          ctx.font = `${10 / Math.max(zm, 0.8)}px ui-monospace, monospace`;
          ctx.fillText(
            `${(errRate * 100).toFixed(0)}% err`,
            n.x,
            n.y + r + 26 / Math.max(zm, 0.8),
          );
        }
        ctx.textAlign = "start";
        ctx.globalAlpha = 1;
      }

      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, []);

  // screen → world
  const toWorld = useCallback((cx: number, cy: number) => {
    const canvas = canvasRef.current!;
    const rect = canvas.getBoundingClientRect();
    const sx = cx - rect.left;
    const sy = cy - rect.top;
    const w = canvas.clientWidth;
    const h = canvas.clientHeight;
    const { ox, oy, zm } = viewRef.current;
    return { x: (sx - w / 2) / zm + w / 2 - ox, y: (sy - h / 2) / zm + h / 2 - oy };
  }, []);

  const hitNode = useCallback((wx: number, wy: number): SimNode | null => {
    let best: SimNode | null = null;
    let bestD = Infinity;
    for (const n of nodesRef.current.values()) {
      const d = Math.hypot(n.x - wx, n.y - wy);
      if (d < nodeRadius(n) + 5 && d < bestD) {
        best = n;
        bestD = d;
      }
    }
    return best;
  }, []);

  const hitEdge = useCallback((wx: number, wy: number): ApiEdge | null => {
    const { zm } = viewRef.current;
    const threshold = 6 / zm + 2;
    let best: ApiEdge | null = null;
    let bestD = Infinity;
    for (const e of edgesRef.current) {
      const a = nodesRef.current.get(e.src);
      const b = nodesRef.current.get(e.dst);
      if (!a || !b) continue;
      const dx = b.x - a.x;
      const dy = b.y - a.y;
      const len2 = dx * dx + dy * dy || 1;
      const t = Math.max(0, Math.min(1, ((wx - a.x) * dx + (wy - a.y) * dy) / len2));
      const d = Math.hypot(wx - (a.x + t * dx), wy - (a.y + t * dy));
      if (d < threshold && d < bestD) {
        best = e;
        bestD = d;
      }
    }
    return best;
  }, []);

  function onMouseDown(ev: React.MouseEvent) {
    const { x, y } = toWorld(ev.clientX, ev.clientY);
    const n = hitNode(x, y);
    dragRef.current = n
      ? { mode: "node", app: n.app, sx: ev.clientX, sy: ev.clientY }
      : { mode: "pan", sx: ev.clientX, sy: ev.clientY };
  }

  function onMouseMove(ev: React.MouseEvent) {
    const drag = dragRef.current;
    if (drag) {
      const dx = ev.clientX - drag.sx;
      const dy = ev.clientY - drag.sy;
      drag.sx = ev.clientX;
      drag.sy = ev.clientY;
      const { zm } = viewRef.current;
      if (drag.mode === "pan") {
        viewRef.current.ox += dx / zm;
        viewRef.current.oy += dy / zm;
      } else if (drag.app) {
        const n = nodesRef.current.get(drag.app);
        if (n) {
          n.x += dx / zm;
          n.y += dy / zm;
          n.fixed = true;
          hotRef.current = Math.max(hotRef.current, 30);
        }
      }
      return;
    }
    const { x, y } = toWorld(ev.clientX, ev.clientY);
    const n = hitNode(x, y);
    hoverRef.current = n ? n.app : null;
    const canvas = canvasRef.current;
    if (canvas) canvas.style.cursor = n || hitEdge(x, y) ? "pointer" : "default";
  }

  function onMouseUp(ev: React.MouseEvent) {
    const drag = dragRef.current;
    dragRef.current = null;
    if (!drag) return;
    // A click (no meaningful movement) selects.
    const moved = Math.hypot(ev.clientX - drag.sx, ev.clientY - drag.sy) > 3;
    if (moved && drag.mode === "pan") return;
    const { x, y } = toWorld(ev.clientX, ev.clientY);
    const n = hitNode(x, y);
    if (n) {
      setSelection({ kind: "node", app: n.app });
      return;
    }
    const e = hitEdge(x, y);
    if (e) {
      setSelection({ kind: "edge", src: e.src, dst: e.dst });
      return;
    }
    setSelection(null);
  }

  function onWheel(ev: React.WheelEvent) {
    const factor = ev.deltaY > 0 ? 0.92 : 1.08;
    const v = viewRef.current;
    v.zm = Math.max(0.15, Math.min(6, v.zm * factor));
  }

  function onDoubleClick() {
    setSelection(null);
    viewRef.current = { ox: 0, oy: 0, zm: 1 };
    for (const n of nodesRef.current.values()) n.fixed = false;
    hotRef.current = 200;
  }

  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") setSelection(null);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // --- panel data ---
  const selNode = selection?.kind === "node" ? nodesRef.current.get(selection.app) : null;
  const selEdge =
    selection?.kind === "edge"
      ? edgesRef.current.find((e) => e.src === selection.src && e.dst === selection.dst)
      : null;
  const nodeEdges = selNode
    ? edgesRef.current.filter((e) => e.src === selNode.app || e.dst === selNode.app)
    : [];

  return (
    <div className="topo">
      <div className="topo-bar">
        <span className="muted">window</span>
        {WINDOWS.map((v) => (
          <button key={v} className={v === win ? "winbtn on" : "winbtn"} onClick={() => setWin(v)}>
            {v}
          </button>
        ))}
        <span className="muted">export</span>
        <a
          className="winbtn"
          href={`/api/v1/topology/export?format=mermaid&window=${win}${apiKeyParam()}`}
          target="_blank"
          rel="noreferrer"
        >
          Mermaid
        </a>
        <a
          className="winbtn"
          href={`/api/v1/topology/export?format=markdown&window=${win}${apiKeyParam()}`}
          target="_blank"
          rel="noreferrer"
        >
          Markdown
        </a>
        <span className="muted topo-hint">
          drag to pan · wheel to zoom · click a service or an edge · double-click to reset
        </span>
      </div>
      {error && <div className="error">{error}</div>}
      <div className="topo-body">
        <canvas
          ref={canvasRef}
          className="topo-canvas"
          onMouseDown={onMouseDown}
          onMouseMove={onMouseMove}
          onMouseUp={onMouseUp}
          onMouseLeave={() => {
            dragRef.current = null;
            hoverRef.current = null;
          }}
          onWheel={onWheel}
          onDoubleClick={onDoubleClick}
        />
        {empty && (
          <div className="muted topo-empty">
            The map builds itself from logs: send entries with an app name (and trace/correlation
            ids for edges) and services appear here.
          </div>
        )}

        {selNode && (
          <div className="topo-panel">
            <div className="topo-title">{selNode.app}</div>
            <div className="kv">
              <span>entries</span>
              <b>{selNode.count.toLocaleString()}</b>
            </div>
            <div className="kv">
              <span>errors</span>
              <b className={selNode.errors > 0 ? "bad" : ""}>{selNode.errors.toLocaleString()}</b>
            </div>
            <div className="kv">
              <span>first seen</span>
              <b>{new Date(selNode.first_seen).toLocaleString()}</b>
            </div>
            <div className="kv">
              <span>last seen</span>
              <b>{new Date(selNode.last_seen).toLocaleString()}</b>
            </div>
            {nodeEdges.length > 0 && (
              <div className="topo-links">
                {nodeEdges.map((e) => (
                  <div
                    key={`${e.src}→${e.dst}`}
                    className="topo-link"
                    onClick={() => setSelection({ kind: "edge", src: e.src, dst: e.dst })}
                  >
                    {e.src === selNode.app ? `→ ${e.dst}` : `← ${e.src}`}
                    <span className="muted"> {e.rps > 0 ? `${e.rps.toFixed(1)} rps` : ""}</span>
                  </div>
                ))}
              </div>
            )}
            <div className="topo-actions">
              <button onClick={() => onOpenLogs(selNode.app, false)}>Logs</button>
              <button onClick={() => onOpenLogs(selNode.app, true)}>Live tail</button>
            </div>
          </div>
        )}

        {selEdge && (
          <div className="topo-panel">
            <div className="topo-title">
              {selEdge.src} <span className="accent">→</span> {selEdge.dst}
            </div>
            <div className="kv">
              <span>origin</span>
              <b>{selEdge.origin}</b>
            </div>
            <div className="kv">
              <span>rate</span>
              <b>{selEdge.rps > 0 ? `${selEdge.rps.toFixed(2)} rps` : "—"}</b>
            </div>
            <div className="kv">
              <span>error rate</span>
              <b className={selEdge.error_rate > 0 ? "bad" : ""}>
                {(selEdge.error_rate * 100).toFixed(2)}%
              </b>
            </div>
            <div className="kv">
              <span>interactions</span>
              <b>{selEdge.count.toLocaleString()}</b>
            </div>
            <div className="kv">
              <span>last seen</span>
              <b>{new Date(selEdge.last_seen).toLocaleString()}</b>
            </div>
            <div className="topo-actions">
              <button onClick={() => onOpenLogs(`${selEdge.src},${selEdge.dst}`, false)}>Logs</button>
              <button onClick={() => onOpenLogs(`${selEdge.src},${selEdge.dst}`, true)}>
                Live tail
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
