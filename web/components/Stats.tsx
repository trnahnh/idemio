import { stats } from "@/lib/content";
import { Reveal } from "./Reveal";

export function Stats() {
  return (
    <div className="grid gap-px overflow-hidden border-y border-line bg-line sm:grid-cols-2 lg:grid-cols-4">
      {stats.map((s, i) => (
        <Reveal key={s.label} delay={i * 60} className="bg-ink-950 p-6 sm:p-8">
          <div className="font-mono text-2xl tracking-tight text-accent sm:text-3xl">
            {s.value}
          </div>
          <div className="mt-2 text-sm text-text">{s.label}</div>
          <div className="mt-1 text-xs leading-relaxed text-dim">{s.note}</div>
        </Reveal>
      ))}
    </div>
  );
}
