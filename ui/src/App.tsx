import { useCallback, useEffect, useState } from "react";
import LogsView, { type LogsRequest } from "./LogsView";
import Topology from "./Topology";
import Rules from "./Rules";
import Access from "./Access";
import Login from "./Login";

type View = "logs" | "topology" | "rules" | "access";

// Who am I: open (no auth configured), key (bootstrap API key) or user.
type Me = { mode: "open" | "key" | "user"; login?: string; role: "admin" | "member" };

export default function App() {
  const [view, setView] = useState<View>("logs");
  const [logsRequest, setLogsRequest] = useState<LogsRequest | null>(null);
  const [me, setMe] = useState<Me | null | "anon">(null); // null = checking

  const checkAuth = useCallback(async () => {
    try {
      const key = localStorage.getItem("logdoc_api_key");
      const res = await fetch("/api/v1/auth/me", {
        headers: key ? { "X-API-Key": key } : {},
      });
      if (res.status === 401) {
        setMe("anon");
        return;
      }
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setMe(await res.json());
    } catch {
      setMe("anon");
    }
  }, []);

  useEffect(() => {
    checkAuth();
  }, [checkAuth]);

  const logout = () => {
    localStorage.removeItem("logdoc_api_key");
    setView("logs");
    setMe("anon");
  };

  // Topology → "show me the logs of this service / edge".
  const openLogs = useCallback((app: string, tail: boolean) => {
    setLogsRequest((prev) => ({ app, tail, id: (prev?.id ?? 0) + 1 }));
    setView("logs");
  }, []);

  if (me === null) return <div className="wrap muted">loading…</div>;

  const admin = me !== "anon" && me.role === "admin";

  return (
    <div className="wrap">
      <header>
        <img src="/logo.svg" alt="LogDoc" className="logo" />
        <nav className="tabs">
          <button className={view === "logs" ? "tab on" : "tab"} onClick={() => setView("logs")}>
            Logs
          </button>
          <button
            className={view === "topology" ? "tab on" : "tab"}
            onClick={() => setView("topology")}
          >
            Topology
          </button>
          <button className={view === "rules" ? "tab on" : "tab"} onClick={() => setView("rules")}>
            Rules
          </button>
          {me !== "anon" && me.mode !== "open" && (
            <button
              className={view === "access" ? "tab on" : "tab"}
              onClick={() => setView("access")}
            >
              Access
            </button>
          )}
        </nav>
        {me !== "anon" && me.mode !== "open" ? (
          <span className="muted">
            {me.mode === "key" ? "api key" : me.login} · {me.role} ·{" "}
            <span className="clickable" onClick={logout}>
              logout
            </span>
          </span>
        ) : (
          <span className="muted">v2</span>
        )}
      </header>

      {me === "anon" ? (
        <Login onDone={checkAuth} />
      ) : (
        <>
          <div style={{ display: view === "logs" ? "block" : "none" }}>
            <LogsView request={logsRequest} />
          </div>
          {view === "topology" && <Topology onOpenLogs={openLogs} />}
          {view === "rules" && <Rules canEdit={admin} />}
          {view === "access" && <Access mode={me.mode} admin={admin} />}
        </>
      )}
    </div>
  );
}
