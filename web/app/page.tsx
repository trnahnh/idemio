import { Hero } from "@/components/Hero";
import { Stats } from "@/components/Stats";
import { Proof } from "@/components/Proof";
import { Flow } from "@/components/Flow";
import { Curve } from "@/components/Curve";
import { Decisions } from "@/components/Decisions";
import { Principles } from "@/components/Principles";
import { Stack } from "@/components/Stack";
import { Section } from "@/components/Section";
import { Reveal } from "@/components/Reveal";
import { guarantee, repo } from "@/lib/content";

export default function Page() {
  return (
    <main>
      <Hero />
      <Stats />

      <Section
        id="problem"
        index="01"
        title="A timeout is not an answer."
        lead="An agent sends a charge and the connection drops. It cannot tell whether the write landed. Retrying risks charging twice; not retrying risks charging nobody. Every agent framework resolves that by retrying — which is the right call, and it needs somewhere for the duplicate to land harmlessly."
      >
        <div className="grid gap-4 lg:grid-cols-3">
          <Reveal className="rounded-2xl border border-bad/25 bg-bad/[0.04] p-6">
            <div className="font-mono text-[11px] uppercase tracking-[0.16em] text-bad">
              without a layer
            </div>
            <p className="mt-3 text-sm leading-relaxed text-muted">
              The retry is a second write. The customer is charged twice, and the evidence is
              spread across two systems that never agreed on what happened.
            </p>
          </Reveal>

          <Reveal delay={70} className="rounded-2xl border border-line bg-ink-900/60 p-6">
            <div className="font-mono text-[11px] uppercase tracking-[0.16em] text-dim">
              the naive fix
            </div>
            <p className="mt-3 text-sm leading-relaxed text-muted">
              Check whether the key exists, then write. Two concurrent requests both check,
              both find nothing, and both proceed. Check-then-act is a race, not a guard.
            </p>
          </Reveal>

          <Reveal delay={140} className="rounded-2xl border border-accent/30 bg-accent-dim p-6">
            <div className="font-mono text-[11px] uppercase tracking-[0.16em] text-accent">
              what idemio does
            </div>
            <p className="mt-3 text-sm leading-relaxed">
              The database decides. A unique constraint on{" "}
              <code className="font-mono text-accent">(agent_id, idempotency_key)</code> is
              claimed in a transaction that commits before anything is sent downstream.
            </p>
          </Reveal>
        </div>

        <Reveal delay={200}>
          <blockquote className="mt-10 border-l-2 border-accent pl-5 text-lg leading-relaxed text-balance sm:pl-7 sm:text-xl">
            {guarantee}
          </blockquote>
        </Reveal>
      </Section>

      <Section
        id="proof"
        index="02"
        title="Nine retries. One charge."
        lead="Drag the slider and fire. Every request is the same logical write with the same key, sent at the same moment. One wins the claim and executes; the rest replay the stored result without touching the downstream."
      >
        <Proof />
      </Section>

      <Section
        id="how"
        index="03"
        title="Where the guarantee actually lives."
        lead="Six steps, and only one of them is load-bearing. The rest exist to make that one safe to depend on."
      >
        <Flow />
      </Section>

      <Section
        id="measured"
        index="04"
        title="Numbers, not adjectives."
        lead="Measured with an open-loop harness on one machine, because a closed loop sends fewer requests as the system slows and reports percentiles that flatter exactly the case worth measuring. The budget holds to roughly 600 writes per second; the tail breaks first, which is the signature of queueing."
      >
        <Curve />
      </Section>

      <Section
        index="05"
        title="Four decisions the code proved wrong."
        lead="Every design decision here is a dated record. Four of them were superseded — not quietly amended — because building the thing showed the reasoning was wrong. That trail is the most useful part of the repository."
      >
        <Decisions />
      </Section>

      <Section
        index="06"
        title="How correctness is argued."
        lead="A system whose whole value is a guarantee has to be suspicious of its own reporting."
      >
        <Principles />
      </Section>

      <Section index="07" title="What it is built on." lead="Chosen for what happens when each one fails.">
        <Stack />
      </Section>

      <footer className="border-t border-line px-5 py-16 sm:px-8 sm:py-20">
        <div className="mx-auto w-full max-w-6xl">
          <Reveal>
            <h2 className="max-w-2xl text-2xl font-semibold tracking-tight text-balance sm:text-3xl">
              The interesting part is the reasoning, and it is all in the repository.
            </h2>
            <p className="mt-4 max-w-xl text-sm leading-relaxed text-muted">
              Eighteen decision records, a living specification that supersedes a frozen PRD,
              and a test suite whose correctness claims are verified against a downstream
              process that keeps its own ledger.
            </p>

            <div className="mt-8 flex flex-col gap-3 sm:flex-row">
              <a
                href={repo}
                target="_blank"
                rel="noreferrer"
                className="group inline-flex items-center justify-center gap-2 rounded-full bg-accent px-6 py-3 text-sm font-medium text-ink-950 transition-transform duration-200 hover:-translate-y-0.5"
              >
                github.com/trnahnh/idemio
                <span className="transition-transform duration-200 group-hover:translate-x-0.5">
                  →
                </span>
              </a>
            </div>
          </Reveal>

          <div className="mt-14 flex flex-col gap-2 border-t border-line pt-8 font-mono text-[11px] text-dim sm:flex-row sm:items-center sm:justify-between">
            <span>idemio — an idempotent transaction layer for agent-driven writes</span>
            <span>Go · PostgreSQL 18 · at most once</span>
          </div>
        </div>
      </footer>
    </main>
  );
}
