# idemio — landing page

A static showcase for the [idemio](https://github.com/trnahnh/idemio) transaction layer.
Next.js App Router, Tailwind v4, no runtime dependencies beyond React — the whole page
prerenders to static HTML.

## Local

```sh
pnpm install
pnpm dev
```

## Deploying to Vercel

The repository root is a Go project, so **set the Vercel project's Root Directory to
`web`**. Everything else is detected: pnpm from the lockfile, Next.js from the framework
preset, and the build output is static.

## A note on the interactive demo

The "fire" control on the landing page animates what the test suite asserts. It is **not**
calling a live backend, and it says so on the page — the real system needs Postgres with
advisory locks, a connection pooler, a broker, object storage and two background processes,
none of which run on a serverless platform. Showing a hollow API that looked like the real
thing would undercut the one claim the project is actually making.
