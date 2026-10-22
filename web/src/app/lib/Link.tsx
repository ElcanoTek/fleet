"use client";

// <Link> — next/link's replacement (see navigation.tsx for the router). Its own
// module so a test that stubs the router hooks still renders real anchors.

import { type AnchorHTMLAttributes, type MouseEvent, forwardRef, useCallback } from "react";
import { navigate } from "./navigation";

type LinkProps = Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "href"> & {
  href: string;
  replace?: boolean;
  /** Accepted for API compatibility with next/link; there is nothing to prefetch. */
  prefetch?: boolean;
};

/**
 * Link — an <a> that soft-navigates on a plain left click and otherwise
 * behaves exactly like an anchor (new tab, modifier keys, download, target).
 */
export const Link = forwardRef<HTMLAnchorElement, LinkProps>(function Link(
  { href, replace, prefetch, onClick, children, ...rest },
  ref,
) {
  void prefetch;
  const handle = useCallback(
    (e: MouseEvent<HTMLAnchorElement>) => {
      onClick?.(e);
      if (
        e.defaultPrevented ||
        e.button !== 0 ||
        e.metaKey ||
        e.ctrlKey ||
        e.shiftKey ||
        e.altKey ||
        rest.target ||
        rest.download !== undefined
      ) {
        return;
      }
      const url = new URL(href, window.location.href);
      if (url.origin !== window.location.origin || url.pathname.startsWith("/api/")) return;
      e.preventDefault();
      navigate(href, { replace });
    },
    [href, replace, onClick, rest.target, rest.download],
  );
  return (
    <a ref={ref} href={href} onClick={handle} {...rest}>
      {children}
    </a>
  );
});

export default Link;
