import { useState } from "react";

// Login screen: a user session (login + password → JWT) or a raw API key /
// personal token. Whatever succeeds lands in localStorage("logdoc_api_key")
// — every request sends it the same way regardless of kind.

export default function Login({ onDone }: { onDone: () => void }) {
  const [login, setLogin] = useState("");
  const [password, setPassword] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [error, setError] = useState<string | null>(null);

  const submitUser = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      const res = await fetch("/api/v1/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ login, password }),
      });
      const body = await res.json().catch(() => null);
      if (!res.ok) throw new Error(body?.error ?? `HTTP ${res.status}`);
      localStorage.setItem("logdoc_api_key", body.token);
      onDone();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const submitKey = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      const res = await fetch("/api/v1/auth/me", { headers: { "X-API-Key": apiKey } });
      if (!res.ok) throw new Error(res.status === 401 ? "invalid key or token" : `HTTP ${res.status}`);
      localStorage.setItem("logdoc_api_key", apiKey);
      onDone();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="login">
      <form className="login-card" onSubmit={submitUser}>
        <div className="sect">// sign in</div>
        <input
          placeholder="login"
          value={login}
          autoFocus
          autoComplete="username"
          onChange={(e) => setLogin(e.target.value)}
        />
        <input
          placeholder="password"
          type="password"
          value={password}
          autoComplete="current-password"
          onChange={(e) => setPassword(e.target.value)}
        />
        <button type="submit" disabled={!login || !password}>
          Sign in
        </button>
      </form>
      <form className="login-card" onSubmit={submitKey}>
        <div className="sect">// or use an API key / personal token</div>
        <input
          placeholder="API key or ldt_… token"
          value={apiKey}
          onChange={(e) => setApiKey(e.target.value)}
        />
        <button type="submit" disabled={!apiKey}>
          Use key
        </button>
      </form>
      {error && <div className="error">{error}</div>}
    </div>
  );
}
