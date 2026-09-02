import { decisions } from "@/lib/content";
import { Reveal } from "./Reveal";

export function Decisions() {
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      {decisions.map((d, i) => (
        <Reveal
          as="article"
          key={`${d.id}-${i}`}
          delay={i * 70}
          className="group flex flex-col rounded-2xl border border-line bg-ink-900/60 p-5 transition-colors duration-300 hover:border-line-strong sm:p-6"
        >
          <span className="font-mono text-[11px] tracking-wide text-accent">{d.id}</span>

          <div className="mt-4 space-y-4">
            <div>
              <div className="font-mono text-[11px] uppercase tracking-[0.16em] text-dim">
                the decision said
              </div>
              <p className="mt-1.5 text-sm leading-relaxed text-muted line-through decoration-bad/40 decoration-1">
                {d.claimed}
              </p>
            </div>

            <div>
              <div className="font-mono text-[11px] uppercase tracking-[0.16em] text-dim">
                the code showed
              </div>
              <p className="mt-1.5 text-sm leading-relaxed">{d.found}</p>
            </div>
          </div>

          <div className="mt-5 flex items-center gap-2 border-t border-line pt-4">
            <span className="h-1.5 w-1.5 rounded-full bg-accent" />
            <span className="font-mono text-xs text-accent">{d.now}</span>
          </div>
        </Reveal>
      ))}
    </div>
  );
}
