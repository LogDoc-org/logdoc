import { useCallback, useEffect, useRef, useState } from "react";

type Entry = {
  ts: string;
  app?: string;
  src?: string;
  lvl: string;
  pid?: string;
  msg: string;
  fields?: Record<string, string>;
};

const LEVELS = ["DEBUG", "INFO", "LOG", "WARN", "ERROR", "SEVERE", "PANIC"];
const MAX_TAIL_ROWS = 500;

function buildParams(app: string, lvl: string[], q: string, limit: number): URLSearchParams {
  const p = new URLSearchParams();
  app
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
    .forEach((a) => p.append("app", a));
  if (lvl.length > 0) p.set("lvl", lvl.join(","));
  if (q) p.set("q", q);
  if (limit) p.set("limit", String(limit));
  const key = localStorage.getItem("logdoc_api_key");
  if (key) p.set("api_key", key);
  return p;
}

export default function App() {
  const [app, setApp] = useState("");
  const [lvl, setLvl] = useState<string[]>([]);
  const [q, setQ] = useState("");
  const [limit, setLimit] = useState(100);
  const [entries, setEntries] = useState<Entry[]>([]);
  const [tookMs, setTookMs] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [tailing, setTailing] = useState(false);
  const wsRef = useRef<WebSocket | null>(null);

  const search = useCallback(async () => {
    stopTail();
    setError(null);
    try {
      const res = await fetch(`/api/v1/query?${buildParams(app, lvl, q, limit)}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      setEntries(data.entries ?? []);
      setTookMs(data.took_ms ?? null);
    } catch (e) {
      setError(String(e));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [app, lvl, q, limit]);

  function stopTail() {
    wsRef.current?.close();
    wsRef.current = null;
    setTailing(false);
  }

  function startTail() {
    stopTail();
    setError(null);
    setEntries([]);
    setTookMs(null);
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const ws = new WebSocket(`${proto}://${location.host}/api/v1/tail?${buildParams(app, lvl, q, 0)}`);
    ws.onmessage = (ev) => {
      const e: Entry = JSON.parse(ev.data);
      setEntries((prev) => [e, ...prev].slice(0, MAX_TAIL_ROWS));
    };
    ws.onerror = () => setError("WebSocket: connection error");
    ws.onclose = () => setTailing(false);
    wsRef.current = ws;
    setTailing(true);
  }

  useEffect(() => () => stopTail(), []);

  function toggleLevel(name: string) {
    setLvl((prev) => (prev.includes(name) ? prev.filter((l) => l !== name) : [...prev, name]));
  }

  return (
    <div className="wrap">
      <header>
        <h1>
          Log<span className="accent">Doc</span>
        </h1>
        <span className="muted">v2 · S1</span>
      </header>

      <div className="filters">
        <input
          placeholder="app (comma-separated)"
          value={app}
          onChange={(e) => setApp(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && search()}
        />
        <input
          placeholder="search in msg"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && search()}
        />
        <input
          type="number"
          min={1}
          max={10000}
          value={limit}
          title="limit"
          onChange={(e) => setLimit(Number(e.target.value))}
        />
        <button onClick={search}>Search</button>
        <button className={tailing ? "tail on" : "tail"} onClick={tailing ? stopTail : startTail}>
          {tailing ? "■ Stop" : "▶ Live tail"}
        </button>
      </div>

      <div className="levels">
        {LEVELS.map((name) => (
          <label key={name} className={lvl.includes(name) ? `lv ${name} sel` : `lv ${name}`}>
            <input type="checkbox" checked={lvl.includes(name)} onChange={() => toggleLevel(name)} />
            {name}
          </label>
        ))}
      </div>

      {error && <div className="error">{error}</div>}
      {tookMs !== null && (
        <div className="muted stat">
          {entries.length} entries in {tookMs} ms
        </div>
      )}
      {tailing && <div className="muted stat">live: {entries.length} entries</div>}

      <table>
        <thead>
          <tr>
            <th>time</th>
            <th>lvl</th>
            <th>app</th>
            <th>src</th>
            <th>message</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e, i) => (
            <tr key={i}>
              <td className="ts">{new Date(e.ts).toLocaleString()}</td>
              <td>
                <span className={`lv ${e.lvl}`}>{e.lvl}</span>
              </td>
              <td>{e.app}</td>
              <td className="src">{e.src}</td>
              <td className="msg">
                {e.msg}
                {e.fields &&
                  Object.entries(e.fields).map(([k, v]) => (
                    <span key={k} className="field">
                      {k}={v}
                    </span>
                  ))}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {entries.length === 0 && !error && <div className="muted empty">no entries</div>}
    </div>
  );
}
