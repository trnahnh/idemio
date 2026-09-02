"use client";

import type { MouseEvent, ReactNode } from "react";

type Props = {
  to: string;
  className?: string;
  children: ReactNode;
};

export function Jump({ to, className, children }: Props) {
  const onClick = (event: MouseEvent<HTMLAnchorElement>) => {
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;

    const target = document.getElementById(to);
    if (!target) return;

    event.preventDefault();
    target.scrollIntoView();
    target.setAttribute("tabindex", "-1");
    target.focus({ preventScroll: true });
  };

  return (
    <a href={`#${to}`} onClick={onClick} className={className}>
      {children}
    </a>
  );
}
