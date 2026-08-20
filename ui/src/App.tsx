import { useCallback, useState } from "react";
import LogsView, { type LogsRequest } from "./LogsView";
import Topology from "./Topology";
import Rules from "./Rules";

type View = "logs" | "topology" | "rules";

export default function App() {
  const [view, setView] = useState<View>("logs");
  const [logsRequest, setLogsRequest] = useState<LogsRequest | null>(null);

  // Topology → "show me the logs of this service / edge".
  const openLogs = useCallback((app: string, tail: boolean) => {
    setLogsRequest((prev) => ({ app, tail, id: (prev?.id ?? 0) + 1 }));
    setView("logs");
  }, []);

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
        </nav>
        <span className="muted">v2</span>
      </header>

      <div style={{ display: view === "logs" ? "block" : "none" }}>
        <LogsView request={logsRequest} />
      </div>
      {view === "topology" && <Topology onOpenLogs={openLogs} />}
      {view === "rules" && <Rules canEdit />}
    </div>
  );
}
