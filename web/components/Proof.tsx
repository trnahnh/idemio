"use client";

import { useCallback, useEffect, useRef, useState } from "react";

type Outcome = "pending" | "created" | "replayed";

type Attempt = {
  id: number;
  outcome: Outcome;
  at: number;
};

const KEY = "7c9e6679-7425-40de-944b-e07fc1f90ae7";

export function Proof() {
  const [concurrency, setConcurrency] = useState(9);
  const [protectedRun, setProtectedRun] = useState(true);
  const [attempts, setAttempts] = useState<Attempt[]>([]);
  const [running, setRunning] = useState(false);
  const timers = useRef<number[]>([]);

  const clear = useCallback(() => {
    timers.current.forEach((t) => window.clearTimeout(t));
    timers.current = [];
  }, []);

  useEffect(() => clear, [clear]);

  const fire = useCallback(() => {
    clear();
    setRunning(true);
    setAttempts(
      Array.from({ length: concurrency }, (_, id) => ({
        id,
        outcome: "pending" as Outcome,
        at: 0,
      })),
    );

    const winner = Math.floor(Math.random() * concurrency);

    for (let id = 0; id < concurrency; id += 1) {
      const delay = 220 + Math.random() * 620;
      const timer = window.setTimeout(() => {
        setAttempts((prev) =>
          prev.map((a) =>
            a.id === id
              ? {
                  ...a,
                  at: Math.round(delay),
                  outcome: protectedRun
                    ? id === winner
                      ? "created"
                      : "replayed"
                    : "created",
                }
              : a,
          ),
        );
      }, delay);
      timers.current.push(timer);
    }

    const done = window.setTimeout(() => setRunning(false), 1000);
    timers.current.push(done);
  }, [clear, concurrency, protectedRun]);

  const executions = protectedRun
    ? attempts.some((a) => a.outcome === "created")
      ? 1
      : 0
    : attempts.filter((a) => a.outcome === "created").length;

  const settled = attempts.filter((a) => a.outcome !== "pending").length;

  return (
    <div className="overflow-hidden rounded-2xl border border-line bg-ink-900/60">
      <div className="flex flex-col gap-4 border-b border-line p-5 sm:flex-row sm:items-end sm:justify-between sm:p-6">
        <div className="flex-1">
          <label
            htmlFor="concurrency"
            className="font-mono text-[11px] uppercase tracking-[0.16em] text-dim"
          >
            Concurrent retries of one key
          </label>
          <div className="mt-3 flex items-center gap-4">
            <input
              id="concurrency"
              type="range"
              min={2}
              max={32}
              value={concurrency}
              onChange={(e) => setConcurrency(Number(e.target.value))}
              className="h-6 w-full max-w-xs cursor-pointer appearance-none bg-transparent [&::-webkit-slider-runnable-track]:h-1 [&::-webkit-slider-runnable-track]:rounded-full [&::-webkit-slider-runnable-track]:bg-ink-700 [&::-webkit-slider-thumb]:mt-[-6px] [&::-webkit-slider-thumb]:h-4 [&::-webkit-slider-thumb]:w-4 [&::-webkit-slider-thumb]:appearance-none [&::-webkit-slider-thumb]:rounded-full [&::-webkit-slider-thumb]:bg-accent [&::-moz-range-track]:h-1 [&::-moz-range-track]:rounded-full [&::-moz-range-track]:bg-ink-700 [&::-moz-range-thumb]:h-4 [&::-moz-range-thumb]:w-4 [&::-moz-range-thumb]:border-0 [&::-moz-range-thumb]:rounded-full [&::-moz-range-thumb]:bg-accent"
            />
            <span className="font-mono text-lg tabular-nums text-accent">{concurrency}</span>
          </div>
        </div>

        <div className="flex items-center gap-3">
          <button
            type="button"
            onClick={() => setProtectedRun((v) => !v)}
            aria-pressed={!protectedRun}
            className={`rounded-full border px-4 py-2 font-mono text-xs transition-colors ${
              protectedRun
                ? "border-line-strong text-muted hover:text-text"
                : "border-bad/60 bg-bad/10 text-bad"
            }`}
          >
            {protectedRun ? "idemio on" : "idemio off"}
          </button>
          <button
            type="button"
            onClick={fire}
            disabled={running}
            className="rounded-full bg-accent px-5 py-2 text-sm font-medium text-ink-950 transition-transform duration-200 hover:-translate-y-0.5 disabled:translate-y-0 disabled:opacity-45"
          >
            {running ? "Firing…" : "Fire"}
          </button>
        </div>
      </div>

      <div className="grid gap-px bg-line sm:grid-cols-3">
        <Metric label="Requests" value={attempts.length} />
        <Metric
          label="Claims won"
          value={attempts.filter((a) => a.outcome === "created").length}
          tone={protectedRun ? "accent" : "bad"}
        />
        <Metric
          label="Downstream executions"
          value={executions}
          tone={executions > 1 ? "bad" : "ok"}
          note={
            attempts.length === 0
              ? "press fire"
              : executions > 1
                ? `${executions} charges for one intent`
                : "exactly once"
          }
        />
      </div>

      <div className="p-5 sm:p-6">
        <div className="flex items-center justify-between font-mono text-[11px] text-dim">
          <span>Idempotency-Key: {KEY}</span>
          <span className="tabular-nums">
            {settled}/{attempts.length || 0} settled
          </span>
        </div>

        <ul className="mt-4 grid grid-cols-2 gap-2 sm:grid-cols-4 lg:grid-cols-8">
          {attempts.map((a) => (
            <li
              key={a.id}
              className={`rounded-lg border px-2.5 py-2 font-mono text-[11px] transition-all duration-300 ${
                a.outcome === "pending"
                  ? "border-line bg-ink-850 text-dim"
                  : a.outcome === "created"
                    ? protectedRun
                      ? "border-accent/50 bg-accent-dim text-accent"
                      : "border-bad/50 bg-bad/10 text-bad"
                    : "border-ok/40 bg-ok/5 text-ok"
              }`}
            >
              <div className="flex items-baseline justify-between gap-1">
                <span>
                  {a.outcome === "pending"
                    ? "···"
                    : a.outcome === "created"
                      ? "201"
                      : "200"}
                </span>
                <span className="text-[11px] opacity-60 tabular-nums">
                  {a.at ? `${a.at}ms` : ""}
                </span>
              </div>
              <div className="mt-1 truncate text-[11px] opacity-70">
                {a.outcome === "pending"
                  ? "in flight"
                  : a.outcome === "created"
                    ? "executed"
                    : "replayed"}
              </div>
            </li>
          ))}
          {attempts.length === 0
            ? Array.from({ length: 8 }).map((_, i) => (
                <li
                  key={i}
                  className="rounded-lg border border-dashed border-line px-2.5 py-2 font-mono text-[11px] text-dim/40"
                >
                  <div>—</div>
                  <div className="mt-1 text-[11px]">idle</div>
                </li>
              ))
            : null}
        </ul>

        <p className="mt-5 border-t border-line pt-4 text-xs leading-relaxed text-dim">
          An animation of what the test suite asserts, not a call to a live backend — the real
          thing needs Postgres, a pooler and a downstream process. With the layer off, every
          racer executes: that is the failure mode, and it is the one your customers notice.
        </p>
      </div>
    </div>
  );
}

function Metric({
  label,
  value,
  tone = "default",
  note,
}: {
  label: string;
  value: number;
  tone?: "default" | "accent" | "ok" | "bad";
  note?: string;
}) {
  const color =
    tone === "accent"
      ? "text-accent"
      : tone === "ok"
        ? "text-ok"
        : tone === "bad"
          ? "text-bad"
          : "text-text";

  return (
    <div className="bg-ink-900 p-5 sm:p-6">
      <div className="font-mono text-[11px] uppercase tracking-[0.16em] text-dim">
        {label}
      </div>
      <div className={`mt-2 font-mono text-3xl tabular-nums sm:text-4xl ${color}`}>{value}</div>
      {note ? <div className="mt-1 font-mono text-[11px] text-dim">{note}</div> : null}
    </div>
  );
}
