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
  declared_only?: boolean; // promised by the code, no logs yet
  group?: string; // domain/namespace from the declared graph
  description?: string; // from the declared graph
  // Extra grouping planes (dc, country, provider...); "/" joins values.
  labels?: Record<string, string>;
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
  declared?: boolean; // the code promises this link
  transport?: string;
  evidence?: string;
  links?: number; // cluster view: how many raw edges this group edge stands for
};

type SimNode = ApiNode & {
  x: number;
  y: number;
  vx: number;
  vy: number;
  fixed: boolean;
  degree: number;
  // Cluster view: a supernode standing for a whole domain/namespace.
  kind?: "group";
  members?: number;
  // Board view: chip bounding box (world units).
  cw?: number;
  ch?: number;
  // Domain drill-in: declared-but-silent member pinned into the parking list.
  parked?: boolean;
  // Zone the layout clusters by — the active plane's value (domain/dc/...).
  zone?: string;
};

type Selection = { kind: "node"; app: string } | { kind: "edge"; src: string; dst: string } | null;

type ApiDeploy = { app: string; version: string; ts: string };

// Catalog entry: the declared half of the service card (GET /api/v1/catalog).
type ApiMeta = {
  app: string;
  owner?: string;
  description?: string;
  links?: Record<string, string>;
  tags?: string[];
  source?: "config" | "api";
};

// GET /api/v1/topology/diff — "what changed" vs the previous window.
type ApiDiff = {
  new_services: { app: string; first_seen: string }[];
  silent_services: { app: string; last_seen: string }[];
  new_edges: { src: string; dst: string }[];
  silent_edges: { src: string; dst: string; last_seen: string }[];
  error_jumps: { src: string; dst: string; prev_error_rate: number; cur_error_rate: number }[];
  deploys: ApiDeploy[];
};

const ACCENT = "#e35b28";
// Visual language ported from the logdoc.org topology demo: infra types get
// their own colors and stay small/unlabeled, services carry the accent.
const CAT_COLORS: Record<string, string> = {
  databases: "#4a9eda",
  "kafka topics": "#9a6ee8",
  "kafka clusters": "#7c53d1",
  redis: "#c94f7c",
  clickhouse: "#e0b93f",
};
const EDGE_COLORS: Record<string, string> = {
  kafka: "#9a6ee8",
  sql: "#4a9eda",
  redis: "#c94f7c",
  nats: "#58b7a5",
  s3: "#b0894e",
  grpc: "#6fbf73",
};
const isInfraNode = (n: { group?: string }) => !!n.group && n.group in CAT_COLORS;
const nodeColor = (n: { group?: string }) => (n.group && CAT_COLORS[n.group]) || ACCENT;
const BAD = "#ff4f4f"; // alarm red, deliberately far from the accent orange
const BAD_RATE = 0.05; // error rate above which a node/edge is drawn as failing
const WINDOWS = ["5m", "15m", "1h", "24h"];

function apiKeyParam(): string {
  const key = localStorage.getItem("logdoc_api_key");
  return key ? `&api_key=${encodeURIComponent(key)}` : "";
}

function authHeaders(): Record<string, string> {
  const key = localStorage.getItem("logdoc_api_key");
  return key ? { "X-API-Key": key } : {};
}

// parseLinks turns "name url" lines into a links map.
function parseLinks(text: string): { links: Record<string, string>; error?: string } {
  const links: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    const sp = line.indexOf(" ");
    if (sp <= 0) return { links, error: `link "${line}": want "name url"` };
    links[line.slice(0, sp).trim()] = line.slice(sp + 1).trim();
  }
  return { links };
}

// Snapshot viewer (shared HTML export): read-only topology, no log access,
// no window/export controls.
const VIEWER = typeof window !== "undefined" && !!(window as unknown as { __SNAP__?: unknown }).__SNAP__;

function nodeRadius(n: SimNode): number {
  if (n.kind === "group") {
    return Math.min(12 + Math.sqrt(n.members ?? 1) * 1.8, 34);
  }
  // Infra nodes stay small (the demo's proportions): services dominate.
  const base = isInfraNode(n) ? 4.5 : 7;
  return Math.min(base + Math.sqrt(n.degree) * 0.8 + Math.log10(1 + n.count) * 0.6, 16);
}

export default function Topology({
  onOpenLogs,
  canEdit = false,
}: {
  onOpenLogs: (app: string, tail: boolean) => void;
  canEdit?: boolean;
}) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const nodesRef = useRef<Map<string, SimNode>>(new Map());
  const edgesRef = useRef<ApiEdge[]>([]);
  const viewRef = useRef({ ox: 0, oy: 0, zm: 1 });
  const hotRef = useRef(400); // remaining simulation ticks before sleep
  const fitRef = useRef(false); // fit-to-view once the layout cools down
  const hoverRef = useRef<string | null>(null);
  const selectionRef = useRef<Selection>(null);
  const dragRef = useRef<{ mode: "pan" | "node"; app?: string; sx: number; sy: number } | null>(null);

  const [selection, setSelection] = useState<Selection>(() => {
    const q = new URLSearchParams(location.search);
    const sel = q.get("sel");
    if (sel) return { kind: "node", app: sel };
    const edge = q.get("edge");
    if (edge && edge.includes("~")) {
      const [src, dst] = edge.split("~");
      return { kind: "edge", src, dst };
    }
    return null;
  });
  const [deploys, setDeploys] = useState<ApiDeploy[]>([]);
  const [catalog, setCatalog] = useState<Record<string, ApiMeta>>({});
  // Edit form state; null = not editing.
  const [metaForm, setMetaForm] = useState<{
    owner: string;
    description: string;
    links: string;
    tags: string;
    error?: string;
  } | null>(null);
  const [showChanges, setShowChanges] = useState(false);
  const projFileRef = useRef<HTMLInputElement | null>(null);
  // Node card tab: Info (the usual card) or Impact (downtime blast radius).
  const [cardTab, setCardTab] = useState<"info" | "impact">("info");
  // Blast radius painted on the canvas while the Impact tab is open —
  // or while an outage simulation (whole plane value down) is active.
  const impactRef = useRef<{
    center: string;
    fail: Set<string>;
    stale: Set<string>;
    deps: Set<string>;
    casc: Set<string>;
  } | null>(null);
  const [diff, setDiff] = useState<ApiDiff | null>(null);
  const [win, setWin] = useState(() => {
    const w = new URLSearchParams(location.search).get("win");
    return w && WINDOWS.includes(w) ? w : "5m";
  });
  const [error, setError] = useState<string | null>(null);
  const [empty, setEmpty] = useState(false);
  // Bump to re-render the panel when polled data changes.
  const [, setDataTick] = useState(0);

  selectionRef.current = selection;

  // Raw topology as the API returned it; the canvas renders a VIEW of it
  // (full graph, domain clusters, or one expanded domain).
  const rawRef = useRef<{ nodes: ApiNode[]; edges: ApiEdge[] }>({ nodes: [], edges: [] });
  // The whole view state lives in the URL: copy the address bar and a
  // colleague opens the exact same screen (same focus, tabs, selection).
  const initQ = useRef(new URLSearchParams(location.search)).current;
  const [grouping, setGrouping] = useState<boolean | null>(
    initQ.get("mode") === "all" ? false : null, // null = decide on first load
  );
  const [expanded, setExpanded] = useState<string | null>(initQ.get("domain"));
  // Board view: business functions as swimlane columns of service chips.
  const [board, setBoard] = useState(initQ.get("mode") === "board");
  const [search, setSearch] = useState("");
  // Focus (ego) view: one service and its neighborhood, laid out in layers —
  // callers to the left, callees to the right.
  const [focus, setFocus] = useState<string | null>(initQ.get("focus"));
  const [hops, setHops] = useState(() => {
    const h = Number(initQ.get("hops"));
    return h >= 1 && h <= 3 ? h : 2;
  });
  const [hideInfra, setHideInfra] = useState(initQ.get("noinfra") === "1");
  // Legend filters: node categories toggled off.
  const [hiddenCats, setHiddenCats] = useState<string[]>(
    initQ.get("hide")?.split(",").filter(Boolean) ?? [],
  );
  // Search suggestions (autocomplete over service names).
  const [suggest, setSuggest] = useState<ApiNode[]>([]);
  // Inline live tail strip under the map: JetBrains-style tabs, one per
  // service; every open tab keeps its own WebSocket and buffer.
  type TailLine = { ts: string; lvl: string; app: string; msg: string; trace?: string; peer?: string };
  const [tails, setTails] = useState<string[]>(() => {
    return new URLSearchParams(location.search).get("tails")?.split(",").filter(Boolean) ?? [];
  });
  const [activeTail, setActiveTail] = useState<string | null>(() => {
    const q = new URLSearchParams(location.search);
    const list = q.get("tails")?.split(",").filter(Boolean) ?? [];
    return q.get("tab") ?? list[list.length - 1] ?? null;
  });
  const [tailBufs, setTailBufs] = useState<Record<string, TailLine[]>>({});
  const tailSocketsRef = useRef<Map<string, WebSocket>>(new Map());
  const tailFor = activeTail; // zoom offset & co use "is the strip open"

  const addTail = useCallback((app: string) => {
    setTails((prev) => (prev.includes(app) ? prev : [...prev, app]));
    setActiveTail(app);
  }, []);

  const closeTail = (app: string) => {
    setTails((prev) => {
      const next = prev.filter((a) => a !== app);
      setActiveTail((cur) => (cur === app ? (next[next.length - 1] ?? null) : cur));
      return next;
    });
    setTailBufs((prev) => {
      const next = { ...prev };
      delete next[app];
      return next;
    });
  };
  const [tailHeight, setTailHeight] = useState(190);
  const [tailFont, setTailFont] = useState(11);
  const tailBodyRef = useRef<HTMLDivElement | null>(null);

  // Keep the newest line visible.
  useEffect(() => {
    const el = tailBodyRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [tailBufs, activeTail]);

  // Drag the tail's top edge to resize it.
  const startTailResize = (ev: React.PointerEvent) => {
    ev.preventDefault();
    const startY = ev.clientY;
    const startH = tailHeight;
    const onMove = (e: PointerEvent) => {
      const h = Math.max(120, Math.min(window.innerHeight * 0.7, startH + (startY - e.clientY)));
      setTailHeight(h);
    };
    const onUp = () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
  };

  const groupOf = (n: ApiNode) => n.group || "other";
  const INFRA_GROUPS = new Set(["databases", "kafka topics", "kafka clusters", "redis", "clickhouse"]);

  // Grouping plane: the same services can be sliced by business domain,
  // by datacenter, by provider... — any label key becomes a plane.
  const [plane, setPlane] = useState<string>(
    () => new URLSearchParams(location.search).get("plane") ?? "domain",
  );
  const clusterOf = (n: ApiNode) =>
    plane === "domain" ? groupOf(n) : n.labels?.[plane] || `no ${plane}`;
  // Outage simulation: one plane value (e.g. dc "DM") goes dark.
  const [sim, setSim] = useState<{ key: string; val: string } | null>(() => {
    const s = new URLSearchParams(location.search).get("sim");
    if (!s || !s.includes(":")) return null;
    const idx = s.indexOf(":");
    return { key: s.slice(0, idx), val: s.slice(idx + 1) };
  });

  // computeView projects the raw graph into what the canvas shows.
  // The optional layout pins nodes to fixed positions (focus view).
  const computeView = useCallback(
    (raw: {
      nodes: ApiNode[];
      edges: ApiEdge[];
    }): {
      nodes: ApiNode[];
      edges: ApiEdge[];
      layout?: Map<string, { x: number; y: number }>;
      board?: { columns: { g: string; x: number; count: number }[]; chips: Map<string, number> };
    } => {
      if (hiddenCats.length > 0) {
        const hide = new Set(hiddenCats);
        const catOf = (n: ApiNode) => (isInfraNode(n) ? (n.group as string) : "service");
        const kept = new Set(raw.nodes.filter((n) => !hide.has(catOf(n))).map((n) => n.app));
        raw = {
          nodes: raw.nodes.filter((n) => kept.has(n.app)),
          edges: raw.edges.filter((e) => kept.has(e.src) && kept.has(e.dst)),
        };
      }
      if (board && !focus) {
        // Swimlane board: column per group, services as ordered chips.
        const canvas = canvasRef.current;
        const w = canvas ? canvas.clientWidth : 1200;
        const groupsOf = new Map<string, ApiNode[]>();
        for (const n of raw.nodes) {
          const g = clusterOf(n);
          groupsOf.set(g, [...(groupsOf.get(g) ?? []), n]);
        }
        const groups = [...groupsOf.keys()].sort();
        const left = 292; // clear of the sidebar overlay
        const colW = Math.max(170, (w - left - 20) / Math.max(groups.length, 1));
        const layout = new Map<string, { x: number; y: number }>();
        const chips = new Map<string, number>(); // app -> chip width
        const columns: { g: string; x: number; count: number }[] = [];
        const errRate = (n: ApiNode) => (n.count > 0 ? n.errors / n.count : 0);
        groups.forEach((g, gi) => {
          const x = left + colW * gi + colW / 2;
          const list = (groupsOf.get(g) ?? []).sort(
            (p1, p2) =>
              errRate(p2) - errRate(p1) ||
              Number(p2.description?.includes("SINGLETON") ?? 0) -
                Number(p1.description?.includes("SINGLETON") ?? 0) ||
              p2.count - p1.count ||
              p1.app.localeCompare(p2.app),
          );
          columns.push({ g, x, count: list.length });
          list.forEach((n, i) => {
            layout.set(n.app, { x, y: 64 + i * 24 });
            chips.set(n.app, Math.min(colW - 14, 16 + n.app.length * 6.8));
          });
        });
        return { nodes: raw.nodes.map((n) => ({ ...n })), edges: raw.edges, layout, board: { columns, chips } };
      }
      if (focus) {
        // Ego view: BFS out (callees, rank>0) and in (callers, rank<0).
        let ns = raw.nodes;
        let es = raw.edges;
        if (hideInfra) {
          const byApp = new Map(raw.nodes.map((n) => [n.app, n]));
          const keep = (app: string) => {
            const n = byApp.get(app);
            return app === focus || !n || !INFRA_GROUPS.has(groupOf(n));
          };
          ns = ns.filter((n) => keep(n.app));
          es = es.filter((e) => keep(e.src) && keep(e.dst));
        }
        const out = new Map<string, string[]>();
        const inc = new Map<string, string[]>();
        for (const e of es) {
          out.set(e.src, [...(out.get(e.src) ?? []), e.dst]);
          inc.set(e.dst, [...(inc.get(e.dst) ?? []), e.src]);
        }
        const rank = new Map<string, number>([[focus, 0]]);
        for (const [adj, dir] of [
          [out, 1],
          [inc, -1],
        ] as const) {
          let frontier = [focus];
          for (let depth = 1; depth <= hops; depth++) {
            const next: string[] = [];
            for (const app of frontier) {
              for (const nb of adj.get(app) ?? []) {
                if (!rank.has(nb)) {
                  rank.set(nb, depth * dir);
                  next.push(nb);
                }
              }
            }
            frontier = next;
          }
        }
        const nodes = ns.filter((n) => rank.has(n.app)).map((n) => ({ ...n }));
        const edges = es.filter((e) => rank.has(e.src) && rank.has(e.dst));
        // Layered layout: rank = column, alphabetical within a column.
        const canvas = canvasRef.current;
        const w = canvas ? canvas.clientWidth : 900;
        const h = canvas ? canvas.clientHeight : 600;
        const byRank = new Map<number, ApiNode[]>();
        for (const n of nodes) {
          const r = rank.get(n.app)!;
          byRank.set(r, [...(byRank.get(r) ?? []), n]);
        }
        const layout = new Map<string, { x: number; y: number }>();
        const gap = Math.min(280, (w - 160) / Math.max(1, 2 * hops));
        for (const [r, list] of byRank) {
          list.sort((a, b) => (groupOf(a) + a.app).localeCompare(groupOf(b) + b.app));
          const step = Math.min(52, (h - 100) / Math.max(1, list.length));
          list.forEach((n, i) => {
            layout.set(n.app, {
              x: w / 2 + r * gap,
              y: h / 2 + (i - (list.length - 1) / 2) * step,
            });
          });
        }
        return { nodes, edges, layout };
      }
      if (!grouping) return raw;
      const byApp = new Map(raw.nodes.map((n) => [n.app, n]));
      // One-service domains fold into "misc" so the overview stays readable.
      const sizes = new Map<string, number>();
      for (const n of raw.nodes) {
        const g = clusterOf(n);
        sizes.set(g, (sizes.get(g) ?? 0) + 1);
      }
      const fold = (g: string) => ((sizes.get(g) ?? 0) < 2 ? "misc" : g);
      const gOf = (app: string) => {
        const n = byApp.get(app);
        return n ? fold(clusterOf(n)) : "other";
      };

      if (!expanded) {
        // Domain view: business functions only — infra is not a domain.
        // (Other planes — dc, provider — DO host infra, so it stays.)
        type Agg = { count: number; errors: number; members: number; observed: number };
        const groups = new Map<string, Agg>();
        for (const n of raw.nodes) {
          if (plane === "domain" && isInfraNode(n)) continue;
          const g = fold(clusterOf(n));
          const a = groups.get(g) ?? { count: 0, errors: 0, members: 0, observed: 0 };
          a.count += n.count;
          a.errors += n.errors;
          a.members += 1;
          if (!n.declared_only) a.observed += 1;
          groups.set(g, a);
        }
        const nodes: ApiNode[] = [...groups.entries()].map(([g, a]) => ({
          app: g,
          first_seen: "",
          last_seen: "",
          count: a.count,
          errors: a.errors,
          declared_only: a.observed === 0,
          group: g,
          ...({ kind: "group", members: a.members } as object),
        }));
        const em = new Map<string, ApiEdge>();
        const isInfraApp = (app: string) => {
          const n = byApp.get(app);
          return !!n && isInfraNode(n);
        };
        for (const e of raw.edges) {
          if (plane === "domain" && (isInfraApp(e.src) || isInfraApp(e.dst))) continue;
          const gs = gOf(e.src);
          const gd = gOf(e.dst);
          if (gs === gd) continue;
          const key = `${gs}→${gd}`;
          const agg =
            em.get(key) ??
            ({
              src: gs,
              dst: gd,
              origin: "declared",
              first_seen: "",
              last_seen: "",
              count: 0,
              errors: 0,
              rps: 0,
              error_rate: 0,
              declared: false,
              links: 0,
            } as ApiEdge);
          agg.count += e.count;
          agg.errors += e.errors;
          agg.rps += e.rps;
          agg.links = (agg.links ?? 0) + 1;
          if (e.declared) agg.declared = true;
          if (e.origin !== "declared") agg.origin = "observed";
          em.set(key, agg);
        }
        for (const e of em.values()) {
          e.error_rate = e.count > 0 ? e.errors / e.count : 0;
        }
        return { nodes, edges: [...em.values()] };
      }

      // One domain expanded: its members, the infra they touch (as real
      // nodes), and neighbor business domains as portals.
      const members = raw.nodes.filter((n) =>
        plane === "domain"
          ? !isInfraNode(n) && fold(clusterOf(n)) === expanded
          : fold(clusterOf(n)) === expanded,
      );
      const memberSet = new Set(members.map((n) => n.app));
      const nodes: ApiNode[] = members.map((n) => ({ ...n }));
      const infraShown = new Set<string>();
      const portalAgg = new Map<string, ApiEdge>();
      const edges: ApiEdge[] = [];
      const portals = new Set<string>();
      for (const e of raw.edges) {
        const sIn = memberSet.has(e.src);
        const dIn = memberSet.has(e.dst);
        if (sIn && dIn) {
          edges.push(e);
          continue;
        }
        if (!sIn && !dIn) continue;
        // Infra neighbor: include the node itself, keep the raw edge.
        const otherApp = sIn ? e.dst : e.src;
        const otherNode = byApp.get(otherApp);
        if (otherNode && isInfraNode(otherNode)) {
          if (!infraShown.has(otherApp)) {
            infraShown.add(otherApp);
            nodes.push({ ...otherNode });
          }
          edges.push(e);
          continue;
        }
        const g = sIn ? gOf(e.dst) : gOf(e.src);
        portals.add(g);
        const src = sIn ? e.src : `▸ ${g}`;
        const dst = sIn ? `▸ ${g}` : e.dst;
        const key = `${src}→${dst}`;
        const agg =
          portalAgg.get(key) ??
          ({
            src,
            dst,
            origin: "declared",
            first_seen: "",
            last_seen: "",
            count: 0,
            errors: 0,
            rps: 0,
            error_rate: 0,
            declared: false,
            links: 0,
          } as ApiEdge);
        agg.count += e.count;
        agg.rps += e.rps;
        agg.links = (agg.links ?? 0) + 1;
        if (e.declared) agg.declared = true;
        if (e.origin !== "declared") agg.origin = "observed";
        portalAgg.set(key, agg);
      }
      for (const g of portals) {
        nodes.push({
          app: `▸ ${g}`,
          first_seen: "",
          last_seen: "",
          count: 0,
          errors: 0,
          declared_only: true,
          group: g,
          ...({ kind: "group", members: 0 } as object),
        });
      }
      return { nodes, edges: [...edges, ...portalAgg.values()] };
    },
    [grouping, expanded, focus, hops, hideInfra, hiddenCats, board, plane],
  );

  const boardRef = useRef<{ columns: { g: string; x: number; count: number }[] } | null>(null);

  const rebuildView = useCallback(() => {
    const view = computeView(rawRef.current);
    boardRef.current = view.board ? { columns: view.board.columns } : null;
    const prev = nodesRef.current;
    const next = new Map<string, SimNode>();
    const degree = new Map<string, number>();
    for (const e of view.edges) {
      degree.set(e.src, (degree.get(e.src) ?? 0) + 1);
      degree.set(e.dst, (degree.get(e.dst) ?? 0) + 1);
    }
    const canvas = canvasRef.current;
    const w = canvas ? canvas.clientWidth : 800;
    const h = canvas ? canvas.clientHeight : 500;
    view.nodes.forEach((n, i) => {
      const old = prev.get(n.app);
      const pin = view.layout?.get(n.app);
      const angle = (i / Math.max(view.nodes.length, 1)) * Math.PI * 2;
      const spread = 120 + Math.sqrt(view.nodes.length) * 14;
      next.set(n.app, {
        ...(n as SimNode),
        zone: (n as SimNode).kind === "group" ? n.app : clusterOf(n),
        degree: degree.get(n.app) ?? 0,
        cw: view.board?.chips.get(n.app),
        ch: view.board ? 18 : undefined,
        x: pin ? pin.x : old ? old.x : w / 2 + Math.cos(angle) * spread + Math.random() * 20,
        y: pin ? pin.y : old ? old.y : h / 2 + Math.sin(angle) * spread + Math.random() * 20,
        vx: 0,
        vy: 0,
        fixed: pin ? true : old ? old.fixed : false,
      });
    });
    // Domain drill-in extras: portals become signpost chips (rect hit-box),
    // and declared-but-silent members — no edges to hold them — get parked
    // in a tidy list instead of drifting as a random cloud.
    if (expanded && !view.layout && !view.board) {
      const parked = view.nodes.filter(
        (n) => !(n as SimNode).kind && (degree.get(n.app) ?? 0) === 0,
      );
      const perCol = Math.max(8, Math.ceil(parked.length / 2));
      parked.forEach((n, i) => {
        const sn = next.get(n.app)!;
        sn.x = w * 0.08 - 700 + Math.floor(i / perCol) * 330;
        sn.y = h * 0.22 + (i % perCol) * 30;
        sn.fixed = true;
        sn.parked = true;
      });
      for (const sn of next.values()) {
        if (sn.kind === "group") {
          sn.cw = sn.app.length * 6.6 + 26;
          sn.ch = 24;
        }
      }
    }
    nodesRef.current = next;
    edgesRef.current = view.edges;
    hotRef.current = view.layout
      ? 0 // pinned layout: no simulation needed
      : Math.max(hotRef.current, prev.size === view.nodes.length ? 60 : 400);
    setDataTick((t) => t + 1);
  }, [computeView, expanded]);

  const load = useCallback(async () => {
    try {
      const res = await fetch(`/api/v1/topology?window=${win}${apiKeyParam()}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: { nodes: ApiNode[]; edges: ApiEdge[] } = await res.json();
      rawRef.current = data;
      if (grouping === null) {
        const groups = new Set(data.nodes.map((n) => n.group || ""));
        groups.delete("");
        setGrouping(data.nodes.length > 150 && groups.size > 1);
      }
      setEmpty(data.nodes.length === 0);
      setError(null);
      rebuildView();
    } catch (e) {
      setError(String(e));
    }
  }, [win, grouping, rebuildView]);

  useEffect(() => {
    load();
    const iv = setInterval(load, 10000);
    return () => clearInterval(iv);
  }, [load]);

  // Mirror the view state into the URL so the screen is shareable.
  useEffect(() => {
    const p = new URLSearchParams();
    if (win !== "5m") p.set("win", win);
    if (board) p.set("mode", "board");
    else if (grouping === false) p.set("mode", "all");
    if (expanded) p.set("domain", expanded);
    if (focus) {
      p.set("focus", focus);
      if (hops !== 2) p.set("hops", String(hops));
      if (hideInfra) p.set("noinfra", "1");
    }
    if (hiddenCats.length > 0) p.set("hide", hiddenCats.join(","));
    if (plane !== "domain") p.set("plane", plane);
    if (sim) p.set("sim", `${sim.key}:${sim.val}`);
    if (selection?.kind === "node") p.set("sel", selection.app);
    else if (selection?.kind === "edge") p.set("edge", `${selection.src}~${selection.dst}`);
    if (tails.length > 0) {
      p.set("tails", tails.join(","));
      if (activeTail && activeTail !== tails[tails.length - 1]) p.set("tab", activeTail);
    }
    const qs = p.toString();
    const url = location.pathname + (qs ? `?${qs}` : "");
    if (location.pathname + location.search !== url) history.replaceState(null, "", url);
  }, [win, grouping, expanded, focus, hops, hideInfra, hiddenCats, selection, tails, activeTail, board, plane, sim]);

  // Re-project the view when the mode changes — and only then re-fit the
  // camera. Data polls never touch it, so a manual pan/zoom sticks.
  useEffect(() => {
    fitRef.current = true;
    rebuildView();
  }, [rebuildView]);

  // Catalog metadata for the service cards.
  const loadCatalog = useCallback(() => {
    fetch(`/api/v1/catalog?${apiKeyParam().slice(1)}`)
      .then((res) => (res.ok ? res.json() : { services: [] }))
      .then((data: { services: ApiMeta[] }) => {
        const map: Record<string, ApiMeta> = {};
        for (const m of data.services) map[m.app] = m;
        setCatalog(map);
      })
      .catch(() => {});
  }, []);

  useEffect(loadCatalog, [loadCatalog]);

  // Deploy markers for the selected service (last 24h).
  useEffect(() => {
    setMetaForm(null); // selecting another node closes the edit form
    if (selection?.kind !== "node") {
      setDeploys([]);
      return;
    }
    let dead = false;
    fetch(`/api/v1/deploys?app=${encodeURIComponent(selection.app)}&window=24h&limit=5${apiKeyParam()}`)
      .then((res) => (res.ok ? res.json() : { deploys: [] }))
      .then((data: { deploys: ApiDeploy[] }) => {
        if (!dead) setDeploys(data.deploys);
      })
      .catch(() => {
        if (!dead) setDeploys([]);
      });
    return () => {
      dead = true;
    };
  }, [selection]);

  // "What changed" report for the current window.
  useEffect(() => {
    if (!showChanges) {
      setDiff(null);
      return;
    }
    let dead = false;
    const loadDiff = () => {
      fetch(`/api/v1/topology/diff?window=${win}${apiKeyParam()}`)
        .then((res) => (res.ok ? res.json() : null))
        .then((data: ApiDiff | null) => {
          if (!dead && data) setDiff(data);
        })
        .catch(() => {});
    };
    loadDiff();
    const iv = setInterval(loadDiff, 10000);
    return () => {
      dead = true;
      clearInterval(iv);
    };
  }, [showChanges, win]);

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
            const bothGroups = a.kind === "group" && b.kind === "group";
            if (d > (bothGroups ? 420 : 250)) continue;
            // Domain bubbles repel harder so their labels never overlap.
            const f = bothGroups
              ? Math.min(1700 / (d * d), 2.0)
              : Math.min(400 / (d * d), 1.2);
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
          // Domain bubbles keep longer springs so the overview breathes.
          const rest = a.kind === "group" || b.kind === "group" ? 210 : 100;
          const f = (d - rest) * 0.008;
          a.vx += (dx / d) * f;
          a.vy += (dy / d) * f;
          b.vx -= (dx / d) * f;
          b.vy -= (dy / d) * f;
        }
        // Gentle pull to the center so disconnected parts stay in view,
        // plus a pull toward the node's domain zone (the demo's DC gravity):
        // clusters drift apart into readable clouds instead of one hairball.
        const groups = [...new Set(nodes.map((n) => n.zone ?? n.group ?? "other"))].sort();
        const useZones = groups.length > 1 && (nodes.length > 40 || nodes.some((n) => n.kind === "group"));
        // Numbered groups ("1 · stage") lay out left-to-right as a pipeline;
        // otherwise zones sit on a circle.
        const numbered = groups.filter((g) => /^\d+\s*·/.test(g));
        const pipeline = numbered.length >= 2;
        const zoneXY = new Map<string, { zx: number; zy: number }>();
        if (pipeline) {
          numbered.forEach((g, i) => {
            const t = numbered.length > 1 ? i / (numbered.length - 1) : 0.5;
            zoneXY.set(g, { zx: w * (0.1 + 0.8 * t), zy: h * 0.44 });
          });
          const rest = groups.filter((g) => !zoneXY.has(g));
          rest.forEach((g, i) => {
            const t = rest.length > 1 ? i / (rest.length - 1) : 0.5;
            zoneXY.set(g, { zx: w * (0.15 + 0.7 * t), zy: h * 0.82 });
          });
        } else {
          const zr = Math.min(w, h) * 0.29;
          groups.forEach((g, i) => {
            const ang = (i / groups.length) * Math.PI * 2;
            zoneXY.set(g, { zx: w / 2 + Math.cos(ang) * zr, zy: h / 2 + Math.sin(ang) * zr });
          });
        }
        for (const n of nodes) {
          if (n.kind !== "group" && !pipeline) {
            n.vx += (w / 2 - n.x) * 0.0005;
            n.vy += (h / 2 - n.y) * 0.0005;
          }
          if (useZones && !n.fixed) {
            const z = zoneXY.get(n.zone ?? n.group ?? "other");
            if (z) {
              // Pipeline: hold the column on X, let Y spread out.
              // Domain bubbles pin to their slots almost rigidly so the
              // stage order survives heavy springs.
              const bubble = n.kind === "group";
              const kx = pipeline ? (bubble ? 0.06 : 0.014) : 0.004;
              const ky = pipeline ? (bubble ? 0.06 : 0.0022) : 0.004;
              n.vx += (z.zx - n.x) * kx;
              n.vy += (z.zy - n.y) * ky;
            }
          }
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

      // Fit-to-view once the layout has cooled (or immediately when pinned).
      if (fitRef.current && hotRef.current <= 1 && nodes.length > 0) {
        fitRef.current = false;
        if (boardRef.current) {
          // Board is laid out in screen units already — camera 1:1.
          viewRef.current = { ox: 0, oy: 0, zm: 1 };
        } else {
        let minX = Infinity,
          maxX = -Infinity,
          minY = Infinity,
          maxY = -Infinity;
        for (const n of nodes) {
          minX = Math.min(minX, n.x);
          maxX = Math.max(maxX, n.x);
          minY = Math.min(minY, n.y);
          maxY = Math.max(maxY, n.y);
        }
        // Reserve room on the left for the sidebar overlay. The reserve is
        // ~280 SCREEN px, so convert via a first-pass zoom estimate.
        const bh = maxY - minY + 140;
        const z0 = Math.max(
          0.15,
          Math.min(1.6, Math.min(w / (maxX - minX + 160), h / bh)),
        );
        minX -= 300 / z0;
        const bw = maxX - minX + 160;
        const zoom = Math.max(0.15, Math.min(1.6, Math.min(w / bw, h / bh)));
        viewRef.current = {
          zm: zoom,
          ox: w / 2 - (minX + maxX) / 2,
          oy: h / 2 - (minY + maxY) / 2,
        };
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

      // Domain zones (the demo's DC ellipses): dashed hulls with a label,
      // drawn under everything on large ungrouped views.
      const boardMeta = boardRef.current;
      if (boardMeta && boardMeta.columns.length > 0) {
        const cols = boardMeta.columns;
        const colW = cols.length > 1 ? cols[1].x - cols[0].x : 220;
        const maxRows = Math.max(...cols.map((c) => c.count));
        const colH = 64 + maxRows * 24;
        for (const c of cols) {
          ctx.fillStyle = "rgba(255,255,255,0.018)";
          ctx.beginPath();
          ctx.roundRect(c.x - colW / 2 + 6, 22, colW - 12, colH, 8);
          ctx.fill();
          ctx.fillStyle = `${ACCENT}b0`;
          ctx.font = `bold ${12 / Math.max(zm, 0.7)}px ui-monospace, monospace`;
          ctx.textAlign = "center";
          ctx.fillText(`${c.g} · ${c.count}`, c.x, 42);
        }
      }
      const anyPinned = nodes.length > 0 && nodes[0].fixed && nodes.every((n) => n.fixed);
      if (nodes.length > 40 && !anyPinned && !nodes.some((n) => n.kind === "group")) {
        const zones = new Map<string, { xs: number[]; ys: number[] }>();
        for (const n of nodes) {
          const g = n.zone ?? n.group ?? "other";
          const z = zones.get(g) ?? { xs: [], ys: [] };
          z.xs.push(n.x);
          z.ys.push(n.y);
          zones.set(g, z);
        }
        for (const [g, z] of zones) {
          if (z.xs.length < 3) continue;
          const cxm = z.xs.reduce((s, v) => s + v, 0) / z.xs.length;
          const cym = z.ys.reduce((s, v) => s + v, 0) / z.ys.length;
          const rx = Math.max(...z.xs.map((v) => Math.abs(v - cxm))) + 26;
          const ry = Math.max(...z.ys.map((v) => Math.abs(v - cym))) + 26;
          const zc = CAT_COLORS[g] || ACCENT;
          ctx.beginPath();
          ctx.ellipse(cxm, cym, rx, ry, 0, 0, Math.PI * 2);
          ctx.fillStyle = `${zc}07`;
          ctx.strokeStyle = `${zc}26`;
          ctx.lineWidth = 1.2 / zm;
          ctx.setLineDash([4 / zm, 4 / zm]);
          ctx.fill();
          ctx.stroke();
          ctx.setLineDash([]);
          ctx.fillStyle = `${zc}80`;
          ctx.font = `bold ${11 / Math.max(zm, 0.7)}px ui-monospace, monospace`;
          ctx.textAlign = "center";
          ctx.fillText(`${g} · ${z.xs.length}`, cxm, cym - ry - 8 / zm);
        }
      }
      const neighbors = new Set<string>();
      if (focusApp) {
        neighbors.add(focusApp);
        for (const e of edges) {
          if (e.src === focusApp) neighbors.add(e.dst);
          if (e.dst === focusApp) neighbors.add(e.src);
        }
      }

      // Impact mode: while the card's Impact tab is open, the blast radius
      // owns the canvas — everything outside it fades away.
      const imp = impactRef.current;
      const inBlast = (x: string) =>
        !!imp &&
        (x === imp.center ||
          imp.fail.has(x) ||
          imp.stale.has(x) ||
          imp.deps.has(x) ||
          imp.casc.has(x));

      for (const e of edges) {
        const a = nodesRef.current.get(e.src);
        const b = nodesRef.current.get(e.dst);
        if (!a || !b) continue;
        if (imp) ctx.globalAlpha = inBlast(e.src) && inBlast(e.dst) ? 1 : 0.06;
        const isSel = sel?.kind === "edge" && sel.src === e.src && sel.dst === e.dst;
        const lit = isSel || (focusApp !== null && (e.src === focusApp || e.dst === focusApp));
        const dim = (focusApp !== null || sel?.kind === "edge") && !lit;
        const bad = e.error_rate > BAD_RATE;
        const trColor = (e.transport && EDGE_COLORS[e.transport]) || null;
        if (boardMeta && !lit && !isSel && !(bad && !dim)) {
          ctx.strokeStyle = "rgba(120,126,140,0.05)";
          ctx.lineWidth = 0.8 / zm;
          ctx.beginPath();
          ctx.moveTo(a.x, a.y);
          ctx.lineTo(b.x, b.y);
          ctx.stroke();
          continue;
        }
        ctx.strokeStyle = dim
          ? bad
            ? "rgba(255,79,79,0.2)"
            : "rgba(120,126,140,0.12)"
          : isSel
            ? ACCENT
            : bad
              ? BAD
              : lit
                ? trColor
                  ? `${trColor}dd`
                  : "rgba(227,91,40,0.8)"
                : trColor
                  ? `${trColor}42`
                  : "rgba(120,126,140,0.35)";
        ctx.lineWidth = (isSel ? 2.2 : bad ? 2 : lit ? 1.8 : 1.1) / zm;
        // Failing edges: marching red dashes, so the failure reads as live.
        // Declared-but-unobserved edges: static dashes — the code promises
        // the link, traffic has not confirmed it yet.
        if (bad && !dim) {
          ctx.setLineDash([7 / zm, 5 / zm]);
          ctx.lineDashOffset = -(now / 40) / zm;
        } else if (e.origin === "declared") {
          ctx.setLineDash([4 / zm, 4 / zm]);
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

        // Transport/rate plate on lit edges (the demo's rounded label chip).
        if ((lit || isSel || (bad && !dim)) && zm > 0.4 && (e.transport || e.rps > 0)) {
          const parts: string[] = [];
          if (e.transport) parts.push(e.transport.toUpperCase());
          if (e.rps > 0) parts.push(`${e.rps.toFixed(e.rps < 10 ? 1 : 0)} rps`);
          if (e.error_rate > 0) parts.push(`${(e.error_rate * 100).toFixed(1)}% err`);
          const label = parts.join(" · ");
          const mx = (a.x + b.x) / 2;
          const my = (a.y + b.y) / 2;
          ctx.font = `bold ${9 / zm}px ui-monospace, monospace`;
          const tw = ctx.measureText(label).width;
          ctx.fillStyle = "rgba(22,21,26,0.92)";
          ctx.beginPath();
          ctx.roundRect(mx - tw / 2 - 4 / zm, my - 7 / zm, tw + 8 / zm, 14 / zm, 3 / zm);
          ctx.fill();
          ctx.fillStyle = bad
            ? "rgba(255,122,110,0.95)"
            : trColor
              ? `${trColor}ee`
              : "rgba(216,219,226,0.9)";
          ctx.textAlign = "center";
          ctx.fillText(label, mx, my + 3 / zm);
          ctx.textAlign = "start";
        }
      }
      ctx.globalAlpha = 1;

      // Parking lot for declared-but-silent members of the open domain.
      const parkedNodes = nodes.filter((n) => n.parked);
      if (parkedNodes.length > 0) {
        ctx.font = `12px ui-monospace, monospace`;
        let pMinX = Infinity,
          pMinY = Infinity,
          pMaxX = -Infinity,
          pMaxY = -Infinity;
        for (const n of parkedNodes) {
          pMinX = Math.min(pMinX, n.x);
          pMinY = Math.min(pMinY, n.y);
          pMaxX = Math.max(pMaxX, n.x + 14 + ctx.measureText(n.app).width);
          pMaxY = Math.max(pMaxY, n.y);
        }
        ctx.beginPath();
        ctx.roundRect(pMinX - 18, pMinY - 26, pMaxX - pMinX + 36, pMaxY - pMinY + 44, 8);
        ctx.strokeStyle = "rgba(139,137,146,0.35)";
        ctx.setLineDash([4, 4]);
        ctx.lineWidth = 1 / zm;
        ctx.stroke();
        ctx.setLineDash([]);
        ctx.fillStyle = "rgba(139,137,146,0.8)";
        ctx.fillText("declared · silent in window", pMinX - 8, pMinY - 36);
      }

      for (const n of nodes) {
        const r = nodeRadius(n);
        const isSel = sel?.kind === "node" && sel.app === n.app;
        const inEdgeSel = sel?.kind === "edge" && (sel.src === n.app || sel.dst === n.app);
        const lit = focusApp === null ? sel?.kind !== "edge" || inEdgeSel : neighbors.has(n.app);
        const errRate = n.count > 0 ? n.errors / n.count : 0;
        const bad = errRate > BAD_RATE;
        ctx.globalAlpha = lit ? 1 : 0.25;
        // Impact mode: the blast radius stays lit, the rest fades; each
        // affected node gets a role-colored ring (red = fails, amber =
        // goes stale, blue = the center's own dependency).
        const impRole = !imp
          ? null
          : n.app === imp.center
            ? "center"
            : imp.fail.has(n.app)
              ? "fail"
              : imp.casc.has(n.app)
                ? "casc"
                : imp.stale.has(n.app)
                  ? "stale"
                  : imp.deps.has(n.app)
                    ? "dep"
                    : "off";
        if (impRole) ctx.globalAlpha = impRole === "off" ? 0.12 : 1;
        if (impRole && impRole !== "off") {
          const ring =
            impRole === "center"
              ? "#ffffff"
              : impRole === "fail" || impRole === "casc"
                ? BAD
                : impRole === "stale"
                  ? "#d9a62e"
                  : "#5b8ff7";
          ctx.strokeStyle = ring;
          ctx.lineWidth = (impRole === "center" ? 2 : 1.5) / zm;
          // Cascade victims: dashed red — they die indirectly.
          if (impRole === "casc") ctx.setLineDash([4 / zm, 3 / zm]);
          ctx.beginPath();
          if (n.cw && n.ch) {
            ctx.roundRect(
              n.x - n.cw / 2 - 3 / zm,
              n.y - n.ch / 2 - 3 / zm,
              n.cw + 6 / zm,
              n.ch + 6 / zm,
              5,
            );
          } else {
            ctx.arc(n.x, n.y, r + 4.5 / zm, 0, Math.PI * 2);
          }
          ctx.stroke();
          ctx.setLineDash([]);
        }
        // Failing node: pulsing red halo, unmistakable even at a glance.
        if (bad) {
          const pulse = (Math.sin(now / 260) + 1) / 2; // 0..1
          ctx.strokeStyle = `rgba(255,79,79,${0.55 - 0.35 * pulse})`;
          ctx.lineWidth = 2 / zm;
          ctx.beginPath();
          ctx.arc(n.x, n.y, r + (3 + pulse * 5) / zm, 0, Math.PI * 2);
          ctx.stroke();
        }
        const baseColor = n.kind === "group" ? ACCENT : nodeColor(n);
        if (n.kind === "group" && !(n.members && n.members > 0)) {
          // Portal to a neighbor domain: a dashed signpost pill, visually
          // distinct from service dots — click jumps into that domain.
          const pw = n.cw ?? n.app.length * 6.6 + 26;
          const ph = n.ch ?? 24;
          ctx.beginPath();
          ctx.roundRect(n.x - pw / 2, n.y - ph / 2, pw, ph, ph / 2);
          ctx.fillStyle = "rgba(30,29,35,0.95)";
          ctx.fill();
          ctx.setLineDash([5, 4]);
          ctx.strokeStyle = isSel || hover === n.app ? "#fff" : `${ACCENT}cc`;
          ctx.lineWidth = (isSel || hover === n.app ? 1.6 : 1.2) / zm;
          ctx.stroke();
          ctx.setLineDash([]);
          ctx.fillStyle = hover === n.app ? "#ffb38f" : `${ACCENT}ee`;
          ctx.font = `11px ui-monospace, monospace`;
          ctx.textAlign = "center";
          ctx.fillText(n.app, n.x, n.y + 3.5);
          ctx.globalAlpha = 1;
          continue;
        }
        if (n.cw && n.ch) {
          // Board chip: rounded rect with the name inside.
          const singleton = n.description?.includes("SINGLETON");
          ctx.beginPath();
          ctx.roundRect(n.x - n.cw / 2, n.y - n.ch / 2, n.cw, n.ch, 4);
          ctx.fillStyle = bad ? "rgba(255,79,79,0.16)" : "rgba(36,35,41,0.95)";
          ctx.fill();
          if (n.declared_only) ctx.setLineDash([3, 3]);
          ctx.strokeStyle = isSel
            ? "#fff"
            : bad
              ? BAD
              : singleton
                ? `${ACCENT}cc`
                : lit
                  ? "rgba(139,137,146,0.8)"
                  : "rgba(44,43,51,1)";
          ctx.lineWidth = isSel || bad || singleton ? 1.6 / zm : 1 / zm;
          ctx.stroke();
          ctx.setLineDash([]);
          ctx.fillStyle = bad ? "#ff9d94" : lit ? "#d8dbe2" : "rgba(216,219,226,0.6)";
          ctx.font = `10.5px ui-monospace, monospace`;
          ctx.textAlign = "center";
          const label = n.app.length > 30 ? n.app.slice(0, 29) + "…" : n.app;
          ctx.fillText(label, n.x, n.y + 3.5);
          if (bad && n.count > 0) {
            ctx.fillStyle = "rgba(255,122,110,0.95)";
            ctx.font = `8.5px ui-monospace, monospace`;
            ctx.textAlign = "left";
            ctx.fillText(`${((n.errors / n.count) * 100).toFixed(0)}%`, n.x + n.cw / 2 + 4, n.y + 3);
            ctx.textAlign = "center";
          }
          ctx.globalAlpha = 1;
          continue;
        }
        if (isSel) {
          // Soft halo behind the selected node.
          ctx.beginPath();
          ctx.arc(n.x, n.y, r + 9 / zm, 0, Math.PI * 2);
          ctx.fillStyle = `${baseColor}22`;
          ctx.fill();
        }
        ctx.beginPath();
        ctx.arc(n.x, n.y, r, 0, Math.PI * 2);
        if (n.kind === "group") {
          // Domain bubble: solid fill regardless of declared/observed,
          // slightly translucent so overlapping labels stay readable.
          ctx.fillStyle = bad ? `${BAD}cc` : `${ACCENT}b8`;
          ctx.fill();
          ctx.strokeStyle = `${ACCENT}`;
          ctx.lineWidth = 1.2 / zm;
          ctx.stroke();
        } else if (n.declared_only) {
          // Ghost node: the code promises the service, no logs yet. Infra
          // ghosts get a faint fill so the color still reads.
          if (isInfraNode(n)) {
            ctx.fillStyle = `${baseColor}66`;
            ctx.fill();
          }
          ctx.strokeStyle = `${baseColor}b0`;
          ctx.lineWidth = 1.4 / zm;
          ctx.setLineDash([3 / zm, 3 / zm]);
          ctx.stroke();
          ctx.setLineDash([]);
        } else {
          ctx.fillStyle = bad ? BAD : baseColor;
          ctx.fill();
        }
        if (isSel || hover === n.app) {
          ctx.strokeStyle = "#fff";
          ctx.lineWidth = 1.5 / zm;
          ctx.stroke();
        }
        // Selective labels (the demo's trick against label soup): infra
        // nodes stay unlabeled unless hovered/selected or zoomed way in.
        const denseView = nodes.length > 120;
        const showLabel =
          n.kind === "group" ||
          n.fixed || // pinned focus layout: few nodes, label everything
          isSel ||
          hover === n.app ||
          (lit && focusApp !== null) ||
          (!isInfraNode(n) && (!denseView || n.degree >= 6 || zm > 1.1)) ||
          zm > 1.8;
        if (showLabel) {
          ctx.fillStyle = lit ? "#d8dbe2" : "rgba(216,219,226,0.5)";
          if (n.parked) {
            // Parking list: label sits beside the dot, left-aligned.
            ctx.font = `12px ui-monospace, monospace`;
            ctx.textAlign = "left";
            ctx.fillText(n.app, n.x + r + 7, n.y + 4);
          } else {
            ctx.font = `${(n.kind === "group" ? 13 : 12) / Math.max(zm, 0.8)}px ui-monospace, monospace`;
            ctx.textAlign = "center";
            const label =
              n.kind === "group" && (n.members ?? 0) > 0 ? `${n.app} · ${n.members}` : n.app;
            ctx.fillText(label, n.x, n.y + r + 14 / Math.max(zm, 0.8));
          }
        }
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
      if (n.cw && n.ch) {
        if (Math.abs(wx - n.x) < n.cw / 2 && Math.abs(wy - n.y) < n.ch / 2 + 2) return n;
        continue;
      }
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
    // Grabbing hand while the canvas (or a node) is being dragged.
    if (canvasRef.current) canvasRef.current.style.cursor = "grabbing";
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
    if (canvas) canvas.style.cursor = n || hitEdge(x, y) ? "pointer" : "grab";
  }

  function onMouseUp(ev: React.MouseEvent) {
    if (canvasRef.current) canvasRef.current.style.cursor = "grab";
    const drag = dragRef.current;
    dragRef.current = null;
    if (!drag) return;
    // A click (no meaningful movement) selects.
    const moved = Math.hypot(ev.clientX - drag.sx, ev.clientY - drag.sy) > 3;
    if (moved && drag.mode === "pan") return;
    const { x, y } = toWorld(ev.clientX, ev.clientY);
    const n = hitNode(x, y);
    if (n) {
      // Cluster supernode: drill into the domain instead of selecting.
      if (n.kind === "group") {
        setSelection(null);
        setExpanded(n.group || n.app);
        return;
      }
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
    if (focus) {
      setFocus(null);
      return;
    }
    if (expanded) {
      // Step back to the domain view first; second double-click resets.
      setExpanded(null);
      return;
    }
    viewRef.current = { ox: 0, oy: 0, zm: 1 };
    for (const n of nodesRef.current.values()) n.fixed = false;
    hotRef.current = 200;
  }

  // Search: Enter focuses the first matching service (ego view).
  const jumpTo = (q: string) => {
    const query = q.trim().toLowerCase();
    if (!query) return;
    const names = rawRef.current.nodes;
    const hit =
      names.find((n) => n.app.toLowerCase() === query) ??
      names.find((n) => n.app.toLowerCase().startsWith(query)) ??
      names.find((n) => n.app.toLowerCase().includes(query));
    if (!hit) return;
    setFocus(hit.app);
    setSelection({ kind: "node", app: hit.app });
    setSearch("");
    setSuggest([]);
  };

  // Autocomplete: ranked matches while typing.
  const updateSuggest = (q: string) => {
    setSearch(q);
    const query = q.trim().toLowerCase();
    if (query.length < 2) {
      setSuggest([]);
      return;
    }
    const names = rawRef.current.nodes;
    const starts = names.filter((n) => n.app.toLowerCase().startsWith(query));
    const contains = names.filter(
      (n) => !n.app.toLowerCase().startsWith(query) && n.app.toLowerCase().includes(query),
    );
    setSuggest([...starts, ...contains].slice(0, 8));
  };

  // Inline live tail for the selected service (terminal strip under the map).
  // The snapshot viewer has no log access — never open tails there.
  useEffect(() => {
    if (VIEWER) return;
    if (selection?.kind === "node" && !nodesRef.current.get(selection.app)?.kind) {
      addTail(selection.app);
    }
  }, [selection, addTail]);

  useEffect(() => {
    const sockets = tailSocketsRef.current;
    // Open sockets for new tabs.
    for (const app of tails) {
      if (sockets.has(app)) continue;
      const key = localStorage.getItem("logdoc_api_key");
      const params = new URLSearchParams({ app });
      if (key) params.set("api_key", key);
      const proto = location.protocol === "https:" ? "wss" : "ws";
      const ws = new WebSocket(`${proto}://${location.host}/api/v1/tail?${params}`);
      ws.onmessage = (ev) => {
        try {
          const e = JSON.parse(ev.data);
          setTailBufs((prev) => ({
            ...prev,
            [app]: [
              ...(prev[app] ?? []).slice(-400),
              {
                ts: e.ts ?? "",
                lvl: e.lvl ?? "INFO",
                app: e.app ?? "",
                msg: e.msg ?? "",
                trace: e.fields?.trace_id ?? "",
                peer: e.fields?.["peer.service"] ?? "",
              },
            ],
          }));
        } catch {
          /* ignore malformed frames */
        }
      };
      sockets.set(app, ws);
    }
    // Close sockets for removed tabs.
    for (const [app, ws] of sockets) {
      if (!tails.includes(app)) {
        ws.close();
        sockets.delete(app);
      }
    }
    return undefined;
  }, [tails]);

  // Close every socket on unmount.
  useEffect(() => {
    const sockets = tailSocketsRef.current;
    return () => {
      for (const ws of sockets.values()) ws.close();
      sockets.clear();
    };
  }, []);

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
  const selMeta = selNode ? catalog[selNode.app] : undefined;

  // The declared description often packs "role · owner: x · dc: y ·
  // replicas: z · ⚠ warning" into one line — unpack it into card sections
  // instead of showing the blob twice.
  const declaredKV: [string, string][] = [];
  const declaredWarns: string[] = [];
  const declaredRestParts: string[] = [];
  for (const p of (selNode?.description ?? "").split(" · ")) {
    if (!p.trim()) continue;
    const m = p.match(/^([a-zA-Z_][a-zA-Z_ -]{0,11}):\s*(.+)$/);
    if (m) declaredKV.push([m[1].toLowerCase(), m[2]]);
    else if (p.trim().startsWith("⚠")) declaredWarns.push(p.replace(/\*\*/g, "").trim());
    else declaredRestParts.push(p);
  }
  const stripMd = (s: string) => s.replace(/\*\*/g, "").trim();
  const rawMetaDesc = selMeta?.description ?? "";
  const rawDeclDesc = declaredRestParts.join(" · ");
  const cardDesc = rawMetaDesc || rawDeclDesc;
  const extraDesc =
    rawMetaDesc && rawDeclDesc && !stripMd(rawMetaDesc).includes(stripMd(rawDeclDesc))
      ? rawDeclDesc
      : "";
  // "**bold**" in catalog text renders as actual emphasis.
  const emph = (s: string) =>
    s.split("**").map((seg, i) => (i % 2 ? <b key={i}>{seg}</b> : seg));
  const declaredOwner = declaredKV.find(([k]) => k === "owner")?.[1];
  const cardOwner = selMeta?.owner || declaredOwner;
  // Dependencies: outbound / inbound, services before infra before portals.
  const depRank = (app: string) => {
    if (app.startsWith("▸")) return 2;
    const n = nodesRef.current.get(app);
    return n && isInfraNode(n) ? 1 : 0;
  };
  const depSort = (dir: "out" | "in") => (a: ApiEdge, b: ApiEdge) => {
    const oa = dir === "out" ? a.dst : a.src;
    const ob = dir === "out" ? b.dst : b.src;
    return depRank(oa) - depRank(ob) || oa.localeCompare(ob);
  };
  const depsOut = selNode
    ? nodeEdges.filter((e) => e.src === selNode.app).sort(depSort("out"))
    : [];
  const depsIn = selNode
    ? nodeEdges.filter((e) => e.dst === selNode.app && e.src !== selNode.app).sort(depSort("in"))
    : [];
  const depRow = (e: ApiEdge, other: string) => {
    const on = nodesRef.current.get(other);
    const dotColor = other.startsWith("▸") ? ACCENT : on ? nodeColor(on) : "#8b8992";
    return (
      <div
        key={`${e.src}→${e.dst}`}
        className="topo-link topo-dep"
        onClick={() => setSelection({ kind: "edge", src: e.src, dst: e.dst })}
      >
        <span className="topo-legend-dot" style={{ background: dotColor }} />
        <span className="topo-dep-name">{other}</span>
        <span className="muted topo-dep-meta">
          {e.transport ?? ""}
          {e.rps > 0 ? ` ${e.rps.toFixed(1)}` : ""}
        </span>
      </div>
    );
  };

  // Downtime blast radius, computed from the FULL graph (not the current
  // view): who fails, who goes stale, and what this service itself needs.
  // Transport-aware: sync callers break immediately; kafka flows go stale.
  const computeImpact = (app: string) => {
    const edges = rawRef.current.edges;
    const byAppRaw = new Map(rawRef.current.nodes.map((n) => [n.app, n]));
    const isTopic = (a: string) => {
      const g = byAppRaw.get(a)?.group ?? "";
      return g === "kafka topics" || g === "kafka clusters";
    };
    const ASYNC = new Set(["kafka", "nats"]);
    const syncCallers: { app: string; tr: string }[] = [];
    const staleConsumers = new Set<string>();
    const syncDeps: { app: string; tr: string }[] = [];
    const topics = new Set<string>();
    for (const e of edges) {
      const tr = (e.transport ?? "").toLowerCase();
      if (e.dst === app && !ASYNC.has(tr)) syncCallers.push({ app: e.src, tr });
      if (e.src === app) {
        if (ASYNC.has(tr) && !isTopic(e.dst)) staleConsumers.add(e.dst);
        else if (isTopic(e.dst)) topics.add(e.dst);
        else if (!ASYNC.has(tr)) syncDeps.push({ app: e.dst, tr });
      }
    }
    // Services sharing my topics: honest wording only — edge direction does
    // not tell producer from consumer, both point at the topic.
    const topicPeers = new Set<string>();
    for (const e of edges) {
      if (topics.has(e.dst) && e.src !== app) topicPeers.add(e.src);
    }
    // Failure cascades up the sync call chain (2 more levels).
    const seen = new Set<string>([app, ...syncCallers.map((c) => c.app)]);
    let frontier = new Set(syncCallers.map((c) => c.app));
    const cascade: string[] = [];
    for (let depth = 0; depth < 2 && frontier.size > 0; depth++) {
      const next = new Set<string>();
      for (const e of edges) {
        const tr = (e.transport ?? "").toLowerCase();
        if (!ASYNC.has(tr) && frontier.has(e.dst) && !seen.has(e.src)) {
          seen.add(e.src);
          next.add(e.src);
          cascade.push(e.src);
        }
      }
      frontier = next;
    }
    const uniqCallers = [...new Map(syncCallers.map((c) => [c.app, c])).values()].sort((a, b) =>
      a.app.localeCompare(b.app),
    );
    const uniqDeps = [...new Map(syncDeps.map((c) => [c.app, c])).values()].sort((a, b) =>
      a.app.localeCompare(b.app),
    );
    return {
      syncCallers: uniqCallers,
      cascade: cascade.sort(),
      staleConsumers: [...staleConsumers].sort(),
      topics: [...topics].sort(),
      topicPeers: [...topicPeers].sort(),
      syncDeps: uniqDeps,
      affected: seen.size - 1 + staleConsumers.size,
    };
  };
  const impact = selNode && cardTab === "impact" ? computeImpact(selNode.app) : null;

  // Outage simulation: nodes living ONLY in the dead plane value go dark,
  // multi-homed ones degrade, and failure cascades up sync call chains.
  const simulateOutage = (key: string, val: string) => {
    const dead = new Set<string>();
    const degraded = new Set<string>();
    for (const n of rawRef.current.nodes) {
      const vals = (n.labels?.[key] ?? "")
        .split("/")
        .map((s) => s.trim())
        .filter(Boolean);
      if (!vals.includes(val)) continue;
      (vals.length === 1 ? dead : degraded).add(n.app);
    }
    const ASYNC = new Set(["kafka", "nats"]);
    const seen = new Set(dead);
    let frontier = new Set(dead);
    const casc = new Set<string>();
    while (frontier.size > 0) {
      const next = new Set<string>();
      for (const e of rawRef.current.edges) {
        const tr = (e.transport ?? "").toLowerCase();
        if (!ASYNC.has(tr) && frontier.has(e.dst) && !seen.has(e.src)) {
          seen.add(e.src);
          next.add(e.src);
          casc.add(e.src);
        }
      }
      frontier = next;
    }
    return { dead, degraded, casc };
  };
  const simSets = sim ? simulateOutage(sim.key, sim.val) : null;

  // Mirror into the ref for the canvas draw loop; simulation wins.
  impactRef.current = simSets
    ? {
        center: "",
        fail: simSets.dead,
        stale: simSets.degraded,
        deps: new Set(),
        casc: simSets.casc,
      }
    : impact
      ? {
          center: selNode!.app,
          fail: new Set([...impact.syncCallers.map((c) => c.app), ...impact.cascade]),
          stale: new Set([...impact.staleConsumers, ...impact.topics]),
          deps: new Set(impact.syncDeps.map((c) => c.app)),
          casc: new Set(),
        }
      : null;

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => setCardTab("info"), [selection?.kind === "node" ? selection.app : null]);

  const openMetaForm = () => {
    setMetaForm({
      owner: selMeta?.owner ?? "",
      description: selMeta?.description ?? "",
      links: Object.entries(selMeta?.links ?? {})
        .map(([name, url]) => `${name} ${url}`)
        .join("\n"),
      tags: (selMeta?.tags ?? []).join(", "),
    });
  };

  const saveMeta = async () => {
    if (!selNode || !metaForm) return;
    const { links, error: linkErr } = parseLinks(metaForm.links);
    if (linkErr) {
      setMetaForm({ ...metaForm, error: linkErr });
      return;
    }
    const tags = metaForm.tags
      .split(",")
      .map((t) => t.trim())
      .filter(Boolean);
    const body = {
      owner: metaForm.owner.trim(),
      description: metaForm.description.trim(),
      links,
      tags,
    };
    const empty = !body.owner && !body.description && !Object.keys(links).length && !tags.length;
    try {
      const res = await fetch(`/api/v1/catalog/${encodeURIComponent(selNode.app)}`, {
        method: empty ? "DELETE" : "PUT",
        headers: { "Content-Type": "application/json", ...authHeaders() },
        body: empty ? undefined : JSON.stringify(body),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => null);
        setMetaForm({ ...metaForm, error: data?.error ?? `HTTP ${res.status}` });
        return;
      }
      setMetaForm(null);
      loadCatalog();
    } catch (e) {
      setMetaForm({ ...metaForm, error: String(e) });
    }
  };

  return (
    <div className="topo">
      <div className="topo-bar">
        {grouping !== null && rawRef.current.nodes.length > 30 && (
          <>
            <button
              className={grouping && !expanded && !board ? "winbtn on" : "winbtn"}
              onClick={() => {
                setExpanded(null);
                setFocus(null);
                setBoard(false);
                setGrouping(true);
                setSelection(null);
              }}
            >
              Domains
            </button>
            <button
              className={board && !focus ? "winbtn on" : "winbtn"}
              onClick={() => {
                setExpanded(null);
                setFocus(null);
                setBoard(true);
                setSelection(null);
              }}
            >
              Board
            </button>
            <button
              className={!grouping && !board ? "winbtn on" : "winbtn"}
              onClick={() => {
                setExpanded(null);
                setFocus(null);
                setBoard(false);
                setGrouping(false);
                setSelection(null);
              }}
            >
              All nodes
            </button>
            {expanded && !focus && (
              <button className="winbtn on" onClick={() => setExpanded(null)}>
                ◀ {expanded}
              </button>
            )}
            {focus && (
              <>
                <button className="winbtn on" onClick={() => setFocus(null)}>
                  ◀ ◉ {focus}
                </button>
                {[1, 2, 3].map((h) => (
                  <button
                    key={h}
                    className={hops === h ? "winbtn on" : "winbtn"}
                    onClick={() => setHops(h)}
                  >
                    {h}
                  </button>
                ))}
                <button
                  className={hideInfra ? "winbtn" : "winbtn on"}
                  onClick={() => setHideInfra((v) => !v)}
                >
                  infra
                </button>
              </>
            )}
          </>
        )}
        {(() => {
          // Planes beyond the business domain (dc, provider…) from labels.
          const keys = [
            ...new Set(rawRef.current.nodes.flatMap((n) => Object.keys(n.labels ?? {}))),
          ].sort();
          if (keys.length === 0) return null;
          const vals =
            plane === "domain"
              ? []
              : [
                  ...new Set(
                    rawRef.current.nodes.flatMap((n) =>
                      (n.labels?.[plane] ?? "")
                        .split("/")
                        .map((s) => s.trim())
                        .filter(Boolean),
                    ),
                  ),
                ].sort();
          return (
            <>
              <span className="muted">plane</span>
              {["domain", ...keys].map((k) => (
                <button
                  key={k}
                  className={plane === k ? "winbtn on" : "winbtn"}
                  onClick={() => {
                    setPlane(k);
                    setExpanded(null);
                    setFocus(null);
                    setSelection(null);
                  }}
                >
                  {k}
                </button>
              ))}
              {vals.length > 0 && (
                <>
                  <span className="muted">☠ outage</span>
                  {vals.map((v) => (
                    <button
                      key={v}
                      className={
                        sim?.key === plane && sim.val === v ? "winbtn on sim-on" : "winbtn"
                      }
                      onClick={() =>
                        setSim(sim?.key === plane && sim.val === v ? null : { key: plane, val: v })
                      }
                    >
                      {v}
                    </button>
                  ))}
                </>
              )}
            </>
          );
        })()}
        {!VIEWER && (
          <>
            <span className="muted">window</span>
            {WINDOWS.map((v) => (
              <button
                key={v}
                className={v === win ? "winbtn on" : "winbtn"}
                onClick={() => setWin(v)}
              >
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
            <a
              className="winbtn"
              href={`/api/v1/topology/snapshot?window=${win}${apiKeyParam()}`}
            >
              HTML
            </a>
            <button
              className="winbtn"
              title="Download the whole project (declared graph + catalog) as one JSON file"
              onClick={() => {
                const t = window.prompt("Project title (stored in the file):", "") ?? "";
                const q = t ? `?title=${encodeURIComponent(t)}` : "?";
                window.location.href = `/api/v1/topology/project${q}${apiKeyParam()}`;
              }}
            >
              Project ⬇
            </button>
            {canEdit && (
              <>
                <button
                  className="winbtn"
                  title="Open a project file: replaces the declared graph and the catalog"
                  onClick={() => projFileRef.current?.click()}
                >
                  Open…
                </button>
                <input
                  ref={projFileRef}
                  type="file"
                  accept=".json,application/json"
                  style={{ display: "none" }}
                  onChange={async (e) => {
                    const f = e.target.files?.[0];
                    e.target.value = "";
                    if (!f) return;
                    if (!window.confirm(`Open "${f.name}"? This REPLACES the declared topology and the catalog.`)) return;
                    try {
                      const body = await f.text();
                      const res = await fetch("/api/v1/topology/project", {
                        method: "POST",
                        headers: { "Content-Type": "application/json", ...authHeaders() },
                        body,
                      });
                      const data = await res.json();
                      if (!res.ok) throw new Error(data.error ?? `HTTP ${res.status}`);
                      setExpanded(null);
                      setFocus(null);
                      setBoard(false);
                      setSelection(null);
                      setError(null);
                      load();
                    } catch (err) {
                      setError(`project import: ${String(err)}`);
                    }
                  }}
                />
              </>
            )}
            <button
              className={showChanges ? "winbtn on" : "winbtn"}
              onClick={() => setShowChanges((v) => !v)}
            >
              Changes
            </button>
          </>
        )}
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

        {simSets && sim && (
          <div className="topo-panel topo-changes">
            <div className="topo-title">
              ☠ {sim.key} «{sim.val}» down{" "}
              <span className="clickable muted" onClick={() => setSim(null)}>
                ×
              </span>
            </div>
            <div className="kv">
              <span>down completely</span>
              <b className="bad">{simSets.dead.size}</b>
            </div>
            <div className="kv">
              <span>die in cascade</span>
              <b className="bad">{simSets.casc.size}</b>
            </div>
            <div className="kv">
              <span>degraded (multi-{sim.key})</span>
              <b style={{ color: "#d9a62e" }}>{simSets.degraded.size}</b>
            </div>
            {simSets.dead.size > 0 && (
              <div className="topo-changes-sec">
                <div className="muted">lived only in “{sim.val}” — down</div>
                {[...simSets.dead].sort().map((a) => (
                  <div key={a} className="topo-link" onClick={() => jumpTo(a)}>
                    <span className="bad">✖</span> {a}
                  </div>
                ))}
              </div>
            )}
            {simSets.casc.size > 0 && (
              <div className="topo-changes-sec">
                <div className="muted">die in cascade — sync-depend on the dead</div>
                {[...simSets.casc].sort().map((a) => (
                  <div key={a} className="topo-link" onClick={() => jumpTo(a)}>
                    <span className="bad">⋯</span> {a}
                  </div>
                ))}
              </div>
            )}
            {simSets.degraded.size > 0 && (
              <div className="topo-changes-sec">
                <div className="muted">lost a node, still alive (multi-{sim.key})</div>
                {[...simSets.degraded].sort().map((a) => (
                  <div key={a} className="topo-link" onClick={() => jumpTo(a)}>
                    <span style={{ color: "#d9a62e" }}>◔</span> {a}
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
        {showChanges && diff && (
          <div className="topo-panel topo-changes">
            <div className="topo-title">
              changed <span className="muted">· last {win} vs the {win} before</span>
            </div>
            {diff.new_services.length === 0 &&
              diff.silent_services.length === 0 &&
              diff.new_edges.length === 0 &&
              diff.silent_edges.length === 0 &&
              diff.error_jumps.length === 0 &&
              diff.deploys.length === 0 && <div className="muted">nothing changed</div>}
            {diff.error_jumps.length > 0 && (
              <div className="topo-changes-sec">
                <div className="muted">error jumps</div>
                {diff.error_jumps.map((j) => (
                  <div
                    key={`${j.src}→${j.dst}`}
                    className="topo-link"
                    onClick={() => setSelection({ kind: "edge", src: j.src, dst: j.dst })}
                  >
                    {j.src} → {j.dst}
                    <span className="bad">
                      {" "}
                      {(j.prev_error_rate * 100).toFixed(1)}% → {(j.cur_error_rate * 100).toFixed(1)}%
                    </span>
                  </div>
                ))}
              </div>
            )}
            {diff.deploys.length > 0 && (
              <div className="topo-changes-sec">
                <div className="muted">deploys</div>
                {diff.deploys.map((d) => (
                  <div
                    key={`${d.app}-${d.version}-${d.ts}`}
                    className="topo-link"
                    onClick={() => setSelection({ kind: "node", app: d.app })}
                  >
                    {d.app} <span className="accent">{d.version}</span>
                    <span className="muted"> {new Date(d.ts).toLocaleTimeString()}</span>
                  </div>
                ))}
              </div>
            )}
            {diff.new_services.length > 0 && (
              <div className="topo-changes-sec">
                <div className="muted">new services</div>
                {diff.new_services.map((n) => (
                  <div
                    key={n.app}
                    className="topo-link"
                    onClick={() => setSelection({ kind: "node", app: n.app })}
                  >
                    {n.app}
                  </div>
                ))}
              </div>
            )}
            {diff.silent_services.length > 0 && (
              <div className="topo-changes-sec">
                <div className="muted">went silent</div>
                {diff.silent_services.map((n) => (
                  <div key={n.app} className="topo-link">
                    {n.app}
                    <span className="muted"> last {new Date(n.last_seen).toLocaleTimeString()}</span>
                  </div>
                ))}
              </div>
            )}
            {diff.new_edges.length > 0 && (
              <div className="topo-changes-sec">
                <div className="muted">new links</div>
                {diff.new_edges.map((e) => (
                  <div
                    key={`${e.src}→${e.dst}`}
                    className="topo-link"
                    onClick={() => setSelection({ kind: "edge", src: e.src, dst: e.dst })}
                  >
                    {e.src} → {e.dst}
                  </div>
                ))}
              </div>
            )}
            {diff.silent_edges.length > 0 && (
              <div className="topo-changes-sec">
                <div className="muted">silent links</div>
                {diff.silent_edges.map((e) => (
                  <div key={`${e.src}→${e.dst}`} className="topo-link">
                    {e.src} → {e.dst}
                  </div>
                ))}
              </div>
            )}
          </div>
        )}

        {selNode && selNode.kind !== "group" && (
          <div className="topo-panel">
            <div className="topo-title">{selNode.app}</div>
            {!metaForm && (
              <div className="card-tabs">
                <button className={cardTab === "info" ? "on" : ""} onClick={() => setCardTab("info")}>
                  Info
                </button>
                <button
                  className={cardTab === "impact" ? "on" : ""}
                  onClick={() => setCardTab("impact")}
                >
                  💥 Impact
                </button>
              </div>
            )}
            {(cardTab === "info" || !!metaForm) && !metaForm && (cardDesc || declaredWarns.length > 0 || selMeta?.tags?.length) && (
              <div className="topo-meta">
                {cardDesc && <div className="topo-desc">{emph(cardDesc)}</div>}
                {extraDesc && <div className="topo-desc muted">{emph(extraDesc)}</div>}
                {declaredWarns.map((wl) => (
                  <div key={wl} className="topo-warn">
                    {wl}
                  </div>
                ))}
                {(selMeta?.tags?.length ?? 0) > 0 && (
                  <div className="topo-tags">
                    {selMeta!.tags!.map((t) => (
                      <span key={t} className="topo-tag">
                        {t}
                      </span>
                    ))}
                  </div>
                )}
              </div>
            )}
            {metaForm && (
              <div className="topo-meta-form">
                <input
                  placeholder="owner (team or person)"
                  value={metaForm.owner}
                  onChange={(e) => setMetaForm({ ...metaForm, owner: e.target.value })}
                />
                <textarea
                  placeholder="description"
                  rows={2}
                  value={metaForm.description}
                  onChange={(e) => setMetaForm({ ...metaForm, description: e.target.value })}
                />
                <textarea
                  placeholder={"links, one per line:\nrepo https://github.com/org/app"}
                  rows={3}
                  value={metaForm.links}
                  onChange={(e) => setMetaForm({ ...metaForm, links: e.target.value })}
                />
                <input
                  placeholder="tags, comma-separated"
                  value={metaForm.tags}
                  onChange={(e) => setMetaForm({ ...metaForm, tags: e.target.value })}
                />
                {metaForm.error && <div className="bad">{metaForm.error}</div>}
                <div className="topo-actions">
                  <button onClick={saveMeta}>Save</button>
                  <button onClick={() => setMetaForm(null)}>Cancel</button>
                </div>
              </div>
            )}
            {cardTab === "info" && (cardOwner || selNode.group || declaredKV.length > 0) && (
              <div className="topo-sec">
                {cardOwner && (
                  <div className="kv">
                    <span>owner</span>
                    <b>{cardOwner}</b>
                  </div>
                )}
                {selNode.group && (
                  <div className="kv">
                    <span>domain</span>
                    <b>{selNode.group}</b>
                  </div>
                )}
                {declaredKV
                  .filter(([k]) => k !== "owner")
                  .map(([k, v]) => (
                    <div key={k} className="kv">
                      <span>{k}</span>
                      <b>{v}</b>
                    </div>
                  ))}
                {Object.entries(selNode.labels ?? {})
                  .filter(([k]) => !declaredKV.some(([dk]) => dk === k))
                  .map(([k, v]) => (
                    <div key={`lbl-${k}`} className="kv">
                      <span>{k}</span>
                      <b>{v}</b>
                    </div>
                  ))}
              </div>
            )}
            {cardTab === "info" && selNode.declared_only && (
              <div className="muted topo-meta-note">declared in code · no logs yet</div>
            )}
            {cardTab === "info" && !selNode.declared_only && (
              <div className="topo-sec">
                <div className="topo-sec-title">traffic · {win}</div>
                <div className="kv">
                  <span>entries</span>
                  <b>{selNode.count.toLocaleString()}</b>
                </div>
                <div className="kv">
                  <span>errors</span>
                  <b className={selNode.errors > 0 ? "bad" : ""}>
                    {selNode.errors.toLocaleString()}
                  </b>
                </div>
                <div className="kv">
                  <span>first seen</span>
                  <b>{new Date(selNode.first_seen).toLocaleString()}</b>
                </div>
                <div className="kv">
                  <span>last seen</span>
                  <b>{new Date(selNode.last_seen).toLocaleString()}</b>
                </div>
              </div>
            )}
            {cardTab === "info" && !metaForm && Object.keys(selMeta?.links ?? {}).length > 0 && (
              <div className="topo-meta-links">
                <div className="muted">links</div>
                {Object.entries(selMeta!.links!).map(([name, url]) => (
                  <a key={name} href={url} target="_blank" rel="noreferrer" className="topo-link">
                    {name}
                  </a>
                ))}
              </div>
            )}
            {cardTab === "info" && deploys.length > 0 && (
              <div className="topo-deploys">
                <div className="muted">deploys (24h)</div>
                {deploys.map((d) => (
                  <div key={`${d.version}-${d.ts}`} className="kv">
                    <span className="accent">{d.version}</span>
                    <b>{new Date(d.ts).toLocaleTimeString()}</b>
                  </div>
                ))}
              </div>
            )}
            {cardTab === "info" && (depsOut.length > 0 || depsIn.length > 0) && (
              <div className="topo-sec">
                {depsOut.length > 0 && (
                  <>
                    <div className="topo-sec-title">calls → · {depsOut.length}</div>
                    {depsOut.map((e) => depRow(e, e.dst))}
                  </>
                )}
                {depsIn.length > 0 && (
                  <>
                    <div className="topo-sec-title">← called by · {depsIn.length}</div>
                    {depsIn.map((e) => depRow(e, e.src))}
                  </>
                )}
              </div>
            )}
            {cardTab === "impact" && !metaForm && impact && (
              <div className="topo-impact">
                <div className="topo-sec">
                  <div className="topo-sec-title">
                    if {selNode.app} goes down · {impact.affected} affected
                  </div>
                  <div className="muted topo-impact-note">
                    painted on the map: red ring = fails · amber = data stales · blue = what
                    this service itself needs
                  </div>
                  {declaredWarns.map((wl) => (
                    <div key={wl} className="topo-warn">
                      {wl}
                    </div>
                  ))}
                  {impact.syncCallers.length === 0 && impact.staleConsumers.length === 0 && (
                    <div className="muted topo-meta-note">
                      no synchronous dependents — nothing breaks instantly. Likely a terminal
                      worker/consumer: when it stops, whatever it writes (storage, analytics)
                      quietly goes stale.
                    </div>
                  )}
                </div>
                {impact.syncCallers.length > 0 && (
                  <div className="topo-sec">
                    <div className="topo-sec-title">
                      ✖ fail immediately · {impact.syncCallers.length}
                    </div>
                    <div className="muted topo-impact-note">
                      call it synchronously — requests start erroring
                    </div>
                    {impact.syncCallers.map((c) => (
                      <div key={c.app} className="topo-link topo-dep" onClick={() => jumpTo(c.app)}>
                        <span className="topo-legend-dot" style={{ background: BAD }} />
                        <span className="topo-dep-name">{c.app}</span>
                        <span className="muted topo-dep-meta">{c.tr}</span>
                      </div>
                    ))}
                    {impact.cascade.length > 0 && (
                      <div className="muted topo-impact-note">
                        …and up the call chain: {impact.cascade.join(", ")}
                      </div>
                    )}
                  </div>
                )}
                {impact.staleConsumers.length > 0 && (
                  <div className="topo-sec">
                    <div className="topo-sec-title">
                      ◔ data goes stale · {impact.staleConsumers.length}
                    </div>
                    <div className="muted topo-impact-note">
                      async flows — no outage, but their data stops updating
                    </div>
                    {impact.staleConsumers.map((a) => (
                      <div key={a} className="topo-link topo-dep" onClick={() => jumpTo(a)}>
                        <span className="topo-legend-dot" style={{ background: "#c9a227" }} />
                        <span className="topo-dep-name">{a}</span>
                        <span className="muted topo-dep-meta">kafka</span>
                      </div>
                    ))}
                  </div>
                )}
                {impact.topics.length > 0 && (
                  <div className="topo-sec">
                    <div className="topo-sec-title">≋ shared topics · {impact.topics.length}</div>
                    <div className="muted topo-impact-note">
                      works with {impact.topics.join(", ")}. The graph can't tell producer from
                      consumer here — IF this service is the producer, topic consumers go
                      stale{impact.topicPeers.length > 0
                        ? `: ${impact.topicPeers.join(", ")}`
                        : ""}.
                    </div>
                  </div>
                )}
                {impact.syncDeps.length > 0 && (
                  <div className="topo-sec">
                    <div className="topo-sec-title">
                      ⚑ this service dies if · {impact.syncDeps.length}
                    </div>
                    <div className="muted topo-impact-note">
                      its own hard sync dependencies
                    </div>
                    {impact.syncDeps.map((c) => (
                      <div key={c.app} className="topo-link topo-dep" onClick={() => jumpTo(c.app)}>
                        <span
                          className="topo-legend-dot"
                          style={{
                            background: nodesRef.current.get(c.app)
                              ? nodeColor(nodesRef.current.get(c.app)!)
                              : "#8b8992",
                          }}
                        />
                        <span className="topo-dep-name">{c.app}</span>
                        <span className="muted topo-dep-meta">{c.tr}</span>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            )}
            <div className="topo-actions">
              {focus !== selNode.app && (
                <button onClick={() => setFocus(selNode.app)}>◉ Focus</button>
              )}
              {!VIEWER && (
                <>
                  <button onClick={() => onOpenLogs(selNode.app, false)}>Logs</button>
                  <button onClick={() => onOpenLogs(selNode.app, true)}>Live tail</button>
                </>
              )}
              {canEdit && !metaForm && selMeta?.source !== "config" && (
                <button onClick={openMetaForm}>Edit</button>
              )}
            </div>
            {selMeta?.source === "config" && (
              <div className="muted topo-meta-note">defined in logdoc.yml</div>
            )}
          </div>
        )}

        {rawRef.current.nodes.length > 30 && (
          <div className="topo-side">
            <div className="topo-side-search">
              <span className="muted">$</span>
              <input
                placeholder="grep service…"
                value={search}
                onChange={(e) => updateSuggest(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") jumpTo(suggest[0]?.app ?? search);
                  if (e.key === "Escape") {
                    setSearch("");
                    setSuggest([]);
                  }
                }}
              />
            </div>
            {suggest.length > 0 && (
              <div className="topo-suggest">
                {suggest.map((n) => (
                  <div key={n.app} className="topo-suggest-row" onClick={() => jumpTo(n.app)}>
                    <span
                      className="topo-legend-dot"
                      style={{ background: nodeColor(n) }}
                    />
                    <span className="topo-suggest-name">{n.app}</span>
                    <span className="muted">{n.group}</span>
                  </div>
                ))}
              </div>
            )}
            <div className="topo-side-title">// node type</div>
            {(() => {
              const counts = new Map<string, number>();
              for (const n of rawRef.current.nodes) {
                const c = isInfraNode(n) ? (n.group as string) : "service";
                counts.set(c, (counts.get(c) ?? 0) + 1);
              }
              return ["service", ...Object.keys(CAT_COLORS)]
                .filter((c) => (counts.get(c) ?? 0) > 0)
                .map((c) => {
                  const off = hiddenCats.includes(c);
                  return (
                    <div
                      key={c}
                      className={off ? "topo-legend-row off" : "topo-legend-row"}
                      onClick={() =>
                        setHiddenCats((prev) =>
                          off ? prev.filter((x) => x !== c) : [...prev, c],
                        )
                      }
                    >
                      <input type="checkbox" readOnly checked={!off} />
                      <span
                        className="topo-legend-dot"
                        style={{ background: c === "service" ? ACCENT : CAT_COLORS[c] }}
                      />
                      {c}
                      <span className="topo-legend-cnt">{counts.get(c)}</span>
                    </div>
                  );
                });
            })()}
            <div className="topo-side-hint">
              hover: deps · click: focus
              <br />
              dbl-click / esc: reset
              <br />
              scroll: zoom · drag: pan
            </div>
          </div>
        )}

        <div className="topo-zoom" style={{ bottom: tailFor ? tailHeight + 20 : 12 }}>
          <button
            onClick={() => {
              const v = viewRef.current;
              v.zm = Math.min(6, v.zm * 1.25);
            }}
          >
            +
          </button>
          <button
            onClick={() => {
              const v = viewRef.current;
              v.zm = Math.max(0.15, v.zm * 0.8);
            }}
          >
            −
          </button>
        </div>

        {!VIEWER && tails.length > 0 && activeTail && (
          <div className="topo-tail" style={{ height: tailHeight }}>
            <div className="topo-tail-resize" onPointerDown={startTailResize} />
            <div className="topo-tail-head">
              <span className="accent">$</span> tail <span className="topo-tail-live">● live</span>
              <span className="topo-tail-tabs">
                {tails.map((app) => (
                  <span
                    key={app}
                    className={app === activeTail ? "topo-tail-tab on" : "topo-tail-tab"}
                    onClick={() => setActiveTail(app)}
                  >
                    {app}
                    <span
                      className="topo-tail-tab-x"
                      onClick={(e) => {
                        e.stopPropagation();
                        closeTail(app);
                      }}
                    >
                      ×
                    </span>
                  </span>
                ))}
              </span>
              <span className="topo-tail-close" onClick={() => setTails([])}>
                ✕
              </span>
              <span className="topo-tail-font">
                <button onClick={() => setTailFont((f) => Math.max(9, f - 1))}>A−</button>
                <button onClick={() => setTailFont((f) => Math.min(18, f + 1))}>A+</button>
              </span>
            </div>
            <div className="topo-tail-body" ref={tailBodyRef} style={{ fontSize: tailFont }}>
              {(tailBufs[activeTail] ?? []).length === 0 && (
                <div className="muted">waiting for log entries…</div>
              )}
              {(tailBufs[activeTail] ?? []).map((l, i) => (
                <div key={i} className="topo-tail-line">
                  <span className="muted">[{l.ts.slice(11, 23)}]</span>{" "}
                  <span className={`lvl-${l.lvl.toLowerCase()}`}>{l.lvl.padEnd(5)}</span>{" "}
                  {l.trace && <span className="topo-tail-trace">trace={l.trace.slice(0, 8)} </span>}
                  <span className="topo-tail-app">{l.app}</span>
                  <span className="muted"> — </span>
                  <span className="topo-tail-msg">{l.msg}</span>
                  {l.peer && <span className="topo-tail-peer"> peer={l.peer}</span>}
                </div>
              ))}
            </div>
          </div>
        )}

        {selEdge && (
          <div className="topo-panel">
            <div className="topo-title">
              {selEdge.src} <span className="accent">→</span> {selEdge.dst}
            </div>
            {(() => {
              // One human sentence about what this link IS, by transport.
              const tr = (selEdge.transport ?? "").toLowerCase();
              const dstG = rawRef.current.nodes.find((n) => n.app === selEdge.dst)?.group ?? "";
              const story =
                tr === "kafka"
                  ? dstG === "kafka topics"
                    ? `${selEdge.src} works with the ${selEdge.dst} topic — producer or consumer, see the code evidence below.`
                    : `Async events flow from ${selEdge.src} to ${selEdge.dst} over Kafka: no direct outage on failure, but data goes stale.`
                  : tr === "sql"
                    ? `${selEdge.src} reads/writes the ${selEdge.dst} database — a hard dependency.`
                    : tr === "redis"
                      ? `${selEdge.src} uses ${selEdge.dst} as a cache/fast store.`
                      : tr === "http" || tr === "grpc"
                        ? `${selEdge.src} calls ${selEdge.dst} synchronously over ${tr.toUpperCase()} — its availability depends on this link.`
                        : "";
              return story ? <div className="topo-desc">{story}</div> : null;
            })()}
            {selEdge.evidence && (
              <div className="topo-sec">
                <div className="topo-sec-title">what &amp; why · from code</div>
                <div className="topo-evidence">{selEdge.evidence}</div>
              </div>
            )}
            <div className="topo-sec">
              <div className="kv">
                <span>origin</span>
                <b>
                  {selEdge.declared && selEdge.origin !== "declared"
                    ? `${selEdge.origin} + declared`
                    : selEdge.origin}
                </b>
              </div>
              {selEdge.transport && (
                <div className="kv">
                  <span>transport</span>
                  <b style={{ color: EDGE_COLORS[selEdge.transport] ?? undefined }}>
                    {selEdge.transport}
                  </b>
                </div>
              )}
              {(selEdge.links ?? 0) > 0 && (
                <div className="kv">
                  <span>links inside</span>
                  <b>{selEdge.links}</b>
                </div>
              )}
            </div>
            <div className="topo-sec">
              <div className="topo-sec-title">traffic · {win}</div>
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
            </div>
            {!VIEWER && (
              <div className="topo-actions">
                <button onClick={() => onOpenLogs(`${selEdge.src},${selEdge.dst}`, false)}>
                  Logs
                </button>
                <button onClick={() => onOpenLogs(`${selEdge.src},${selEdge.dst}`, true)}>
                  Live tail
                </button>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
