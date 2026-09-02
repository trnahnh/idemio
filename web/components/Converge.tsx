const LANES = 9;

// Nine retries of the same logical write arrive; one line continues past the constraint.
// The whole product in one figure, so it is drawn rather than described.
export function Converge() {
  const height = 260;
  const gate = 0.56;

  return (
    <figure className="anim relative overflow-hidden rounded-2xl border border-line bg-ink-900/60 p-5 backdrop-blur-sm sm:p-8">
      <figcaption className="flex flex-col gap-1 sm:flex-row sm:items-baseline sm:justify-between">
        <span className="font-mono text-[11px] uppercase tracking-[0.16em] text-dim">
          One key · nine attempts
        </span>
        <span className="font-mono text-[11px] text-dim">
          verified against the downstream ledger
        </span>
      </figcaption>

      <div className="relative mt-6">
        <svg
          viewBox={`0 0 1000 ${height}`}
          className="w-full"
          role="img"
          aria-label="Nine duplicate requests converging on a single constraint, with exactly one execution continuing downstream."
        >
          <defs>
            <linearGradient id="lane" x1="0" x2="1">
              <stop offset="0%" stopColor="#4a5462" stopOpacity="0" />
              <stop offset="35%" stopColor="#4a5462" stopOpacity="0.9" />
              <stop offset="100%" stopColor="#7b8794" stopOpacity="0.95" />
            </linearGradient>
            <linearGradient id="through" x1="0" x2="1">
              <stop offset="0%" stopColor="#f2b53c" />
              <stop offset="100%" stopColor="#f2b53c" stopOpacity="0.55" />
            </linearGradient>
          </defs>

          {Array.from({ length: LANES }).map((_, i) => {
            const y = 22 + i * ((height - 44) / (LANES - 1));
            const midY = height / 2;
            return (
              <path
                key={i}
                d={`M 0 ${y} C ${gate * 620} ${y}, ${gate * 700} ${midY}, ${gate * 1000} ${midY}`}
                fill="none"
                stroke="url(#lane)"
                strokeWidth="1.5"
                strokeDasharray="240"
                style={{
                  animation: `travel 1.5s var(--ease-out-expo) ${i * 0.09}s both`,
                }}
              />
            );
          })}

          <line
            x1={gate * 1000}
            y1="10"
            x2={gate * 1000}
            y2={height - 10}
            stroke="rgba(242,181,60,0.35)"
            strokeWidth="1"
            strokeDasharray="3 5"
          />

          <path
            d={`M ${gate * 1000} ${height / 2} L 1000 ${height / 2}`}
            fill="none"
            stroke="url(#through)"
            strokeWidth="2.5"
            strokeDasharray="240"
            style={{ animation: "travel 1.1s var(--ease-out-expo) 1.15s both" }}
          />

          <circle cx={gate * 1000} cy={height / 2} r="5" fill="#f2b53c" />
          <circle
            cx={gate * 1000}
            cy={height / 2}
            r="5"
            fill="none"
            stroke="#f2b53c"
            strokeWidth="1"
            style={{
              transformOrigin: `${gate * 1000}px ${height / 2}px`,
              animation: "pulse-ring 2.6s ease-out 1.2s infinite",
            }}
          />
        </svg>

        <div
          className="pointer-events-none absolute font-mono text-[11px] leading-tight"
          style={{ left: `${gate * 100}%`, top: 0, transform: "translateX(-50%)" }}
        >
          <span className="whitespace-nowrap text-accent">unique (agent_id, key)</span>
        </div>
      </div>

      <dl className="mt-6 grid grid-cols-3 gap-3 border-t border-line pt-5 sm:gap-6">
        <div>
          <dt className="font-mono text-[11px] uppercase tracking-wider text-dim">
            Requests
          </dt>
          <dd className="mt-1 font-mono text-xl tabular-nums sm:text-2xl">9</dd>
        </div>
        <div>
          <dt className="font-mono text-[11px] uppercase tracking-wider text-dim">
            Claims won
          </dt>
          <dd className="mt-1 font-mono text-xl tabular-nums text-accent sm:text-2xl">1</dd>
        </div>
        <div>
          <dt className="font-mono text-[11px] uppercase tracking-wider text-dim">
            Executions
          </dt>
          <dd className="mt-1 font-mono text-xl tabular-nums text-ok sm:text-2xl">1</dd>
        </div>
      </dl>
    </figure>
  );
}
