"use client";

import { useState } from "react";

// Orchestrator login card. Renders moc's username/password form PLUS the
// "Use Elcano email" handoff — the two login paths the unified gate accepts.
// The password path persists a bearer token (useOrchestratorSession.login);
// the Elcano path bounces to the auth service and comes back with the shared
// cookie (no token needed). This is the orchestrator analogue of chat's
// LoginCard, but with username/password (moc) instead of email/password.

export type OrchestratorLoginProps = {
  elcanoLoginEnabled: boolean;
  onLogin: (username: string, password: string) => Promise<boolean>;
  error: string | null;
};

export function OrchestratorLogin({ elcanoLoginEnabled, onLogin, error }: OrchestratorLoginProps) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  const submit = async () => {
    setSubmitting(true);
    try {
      await onLogin(username, password);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="auth-section" role="region" aria-label="Authentication">
      <div className="auth-fields stack-form">
        <h2>Sign in</h2>
        <p className="caption">Sign in to access the internal Operations Center workspace.</p>

        {error ? (
          <div className="validation-error" role="alert" data-testid="orchestrator-login-error">
            {error}
          </div>
        ) : null}

        <label htmlFor="orch-username">Username</label>
        <input
          id="orch-username"
          type="text"
          autoComplete="username"
          aria-label="Username"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              void submit();
            }
          }}
        />

        <label htmlFor="orch-password">Password</label>
        <div className="password-wrapper">
          <input
            id="orch-password"
            type={showPassword ? "text" : "password"}
            autoComplete="current-password"
            aria-label="Password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                void submit();
              }
            }}
          />
          <button
            type="button"
            className="password-toggle"
            aria-label={showPassword ? "Hide password" : "Show password"}
            onClick={() => setShowPassword((s) => !s)}
          >
            {showPassword ? "Hide" : "Show"}
          </button>
        </div>

        <button
          type="button"
          className="btn btn-primary"
          aria-label="Login with username and password"
          disabled={submitting}
          onClick={() => void submit()}
        >
          {submitting ? "Authenticating…" : "Sign In"}
        </button>

        {elcanoLoginEnabled ? (
          <>
            <div className="auth-divider" aria-hidden="true">
              or
            </div>
            <a
              className="btn btn-secondary"
              href="/api/orchestrator/auth/elcano-login"
              aria-label="Sign in with your Elcano email"
            >
              Use Elcano email
            </a>
          </>
        ) : null}
      </div>
    </div>
  );
}

export default OrchestratorLogin;
