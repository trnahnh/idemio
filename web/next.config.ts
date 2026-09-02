import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // next dev writes AGENTS.md and CLAUDE.md into this directory on every start.
  agentRules: false,
};

export default nextConfig;
