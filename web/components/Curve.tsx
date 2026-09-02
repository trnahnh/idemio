import { budget, latency } from "@/lib/content";

const W = 720;
const H = 300;
const PAD = { top: 20, right: 20, bottom: 40, left: 46 };

const maxRate = 900;
const maxMs = 120;

const x = (rate: number) =>
  PAD.left + (rate / maxRate) * (W - PAD.left - PAD.right);
const y = (ms: number) => H - PAD.bottom - (ms / maxMs) * (H - PAD.top - PAD.bottom);

function path(key: "p50" | "p99") {
  return latency
    .map((d, i) => `${i === 0 ? "M" : "L"} ${x(d.rate).toFixed(1)} ${y(d[key]).toFixed(1)}`)
    .join(" ");
}

// The knee between 600/s and 800/s is the point of the chart, so the budget lines are drawn
// rather than described: you can see where the p99 crosses and the p50 does not.
export function Curve() {
  return (
    <div className="overflow-hidden rounded-2xl border border-line bg-ink-900/60 p-4 sm:p-6">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="font-mono text-[11px] uppercase tracking-[0.16em] text-dim">
          Overhead vs arrival rate
        </span>
        <span className="font-mono text-[11px] text-dim">
          one machine · open loop · 50 resources
        </span>
      </div>

      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="mt-4 hidden w-full sm:block"
        role="img"
        aria-label="Latency overhead against arrival rate. p50 stays under the 15ms budget at every measured rate; p99 crosses the 60ms budget between 600 and 800 requests per second."
      >
        {[0, 30, 60, 90, 120].map((ms) => (
          <g key={ms}>
            <line
              x1={PAD.left}
              x2={W - PAD.right}
              y1={y(ms)}
              y2={y(ms)}
              stroke="rgba(255,255,255,0.06)"
            />
            <text
              x={PAD.left - 10}
              y={y(ms) + 4}
              textAnchor="end"
              className="fill-[#5a636e] font-mono"
              fontSize="11"
            >
              {ms}
            </text>
          </g>
        ))}

        <line
          x1={PAD.left}
          x2={W - PAD.right}
          y1={y(budget.p99)}
          y2={y(budget.p99)}
          stroke="#ff5f57"
          strokeOpacity="0.5"
          strokeDasharray="4 4"
        />
        <text
          x={W - PAD.right}
          y={y(budget.p99) - 7}
          textAnchor="end"
          className="fill-[#ff5f57] font-mono"
          fontSize="10"
        >
          p99 budget · 60ms
        </text>

        <line
          x1={PAD.left}
          x2={W - PAD.right}
          y1={y(budget.p50)}
          y2={y(budget.p50)}
          stroke="#f2b53c"
          strokeOpacity="0.5"
          strokeDasharray="4 4"
        />
        <text
          x={W - PAD.right}
          y={y(budget.p50) - 7}
          textAnchor="end"
          className="fill-[#f2b53c] font-mono"
          fontSize="10"
        >
          p50 budget · 15ms
        </text>

        <path d={path("p99")} fill="none" stroke="#ff8a80" strokeWidth="2" />
        <path d={path("p50")} fill="none" stroke="#4fd1a0" strokeWidth="2" />

        {latency.map((d) => (
          <g key={d.rate}>
            <circle cx={x(d.rate)} cy={y(d.p99)} r="3.5" fill="#ff8a80" />
            <circle cx={x(d.rate)} cy={y(d.p50)} r="3.5" fill="#4fd1a0" />
            <text
              x={x(d.rate)}
              y={H - PAD.bottom + 20}
              textAnchor="middle"
              className="fill-[#5a636e] font-mono"
              fontSize="11"
            >
              {d.rate}/s
            </text>
          </g>
        ))}
      </svg>

      <table className="mt-4 w-full border-collapse font-mono text-[13px] sm:hidden">
        <thead>
          <tr className="text-left text-dim">
            <th className="pb-2 font-normal">rate</th>
            <th className="pb-2 text-right font-normal">p50</th>
            <th className="pb-2 text-right font-normal">p95</th>
            <th className="pb-2 text-right font-normal">p99</th>
          </tr>
        </thead>
        <tbody className="tabular-nums">
          {latency.map((d) => (
            <tr key={d.rate} className="border-t border-line">
              <td className="py-2.5">{d.rate}/s</td>
              <td
                className={`py-2.5 text-right ${d.p50 > budget.p50 ? "text-bad" : "text-ok"}`}
              >
                {d.p50}ms
              </td>
              <td className="py-2.5 text-right text-muted">{d.p95}ms</td>
              <td
                className={`py-2.5 text-right ${d.p99 > budget.p99 ? "text-bad" : "text-muted"}`}
              >
                {d.p99}ms
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-2 border-t border-line pt-4 font-mono text-[11px] text-dim">
        <span className="hidden items-center gap-2 sm:flex">
          <span className="h-0.5 w-4 bg-[#4fd1a0]" /> p50
        </span>
        <span className="hidden items-center gap-2 sm:flex">
          <span className="h-0.5 w-4 bg-[#ff8a80]" /> p99
        </span>
        <span className="ml-auto">
          exactly-once held at every rate — that is what the harness asserts
        </span>
      </div>
    </div>
  );
}
