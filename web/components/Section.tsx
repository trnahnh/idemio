import type { ReactNode } from "react";
import { Reveal } from "./Reveal";

type Props = {
  id?: string;
  index: string;
  title: string;
  lead?: string;
  children: ReactNode;
};

export function Section({ id, index, title, lead, children }: Props) {
  return (
    <section id={id} className="relative border-t border-line px-5 py-20 sm:px-8 sm:py-28">
      <div className="mx-auto w-full max-w-6xl">
        <Reveal>
          <div className="flex items-baseline gap-4">
            <span className="font-mono text-xs text-accent tabular-nums">{index}</span>
            <div className="h-px flex-1 bg-line" />
          </div>
          <h2 className="mt-6 max-w-3xl text-3xl font-semibold tracking-tight text-balance sm:text-4xl md:text-5xl">
            {title}
          </h2>
          {lead ? (
            <p className="mt-5 max-w-2xl text-base leading-relaxed text-muted sm:text-lg">
              {lead}
            </p>
          ) : null}
        </Reveal>
        <div className="mt-12 sm:mt-14">{children}</div>
      </div>
    </section>
  );
}
