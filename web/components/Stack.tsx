import { stack } from "@/lib/content";
import { Reveal } from "./Reveal";

export function Stack() {
  return (
    <div className="grid gap-px overflow-hidden rounded-2xl border border-line bg-line sm:grid-cols-2 lg:grid-cols-4">
      {stack.map((s, i) => (
        <Reveal
          key={s.name}
          delay={i * 45}
          className="flex flex-col bg-ink-900 p-5 transition-colors duration-300 hover:bg-ink-850 sm:p-6"
        >
          <h3 className="font-mono text-sm text-text">{s.name}</h3>
          <p className="mt-1.5 text-xs text-dim">{s.role}</p>
          <p className="mt-4 text-sm leading-relaxed text-muted">{s.why}</p>
        </Reveal>
      ))}
    </div>
  );
}
