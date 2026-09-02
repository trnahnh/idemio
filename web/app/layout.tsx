import type { Metadata, Viewport } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
  display: "swap",
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
  display: "swap",
});

export const metadata: Metadata = {
  metadataBase: new URL("https://idemio.vercel.app"),
  title: "idemio — an idempotent transaction layer for agent-driven writes",
  description:
    "AI agents retry writes they cannot interpret. idemio guarantees a given logical write executes at most once, enforced by a Postgres unique constraint rather than by application logic.",
  keywords: [
    "idempotency",
    "distributed systems",
    "exactly once",
    "at most once",
    "PostgreSQL",
    "Go",
    "AI agents",
  ],
  openGraph: {
    title: "idemio — send it twice, charge them once",
    description:
      "An idempotent transaction layer for agent-driven writes. At-most-once execution enforced by a unique constraint, verified against an independent downstream ledger.",
    type: "website",
  },
  twitter: {
    card: "summary_large_image",
    title: "idemio — send it twice, charge them once",
    description:
      "At-most-once execution for agent-driven writes, enforced by a Postgres unique constraint.",
  },
};

export const viewport: Viewport = {
  themeColor: "#06070a",
  width: "device-width",
  initialScale: 1,
};

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html
      lang="en"
      className={`${geistSans.variable} ${geistMono.variable} h-full antialiased`}
    >
      <body className="min-h-full">{children}</body>
    </html>
  );
}
