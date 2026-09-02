import { principles } from "@/lib/content";
import { Reveal } from "./Reveal";

export function Principles() {
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      {principles.map((p, i) => (
        <Reveal
          key={p.title}
          delay={i * 70}
          className="rounded-2xl border border-line bg-gradient-to-b from-ink-900/80 to-ink-950 p-5 sm:p-7"
        >
          <h3 className="text-lg font-medium tracking-tight text-balance sm:text-xl">
            {p.title}
          </h3>
          <p className="mt-3 text-sm leading-relaxed text-muted sm:text-[15px]">{p.body}</p>
        </Reveal>
      ))}
    </div>
  );
}
