import { Fragment, type ReactNode } from "react";
import { t } from "./index";

/** A complete translated sentence may reorder links and emphasized words.
 * Slots are React nodes, never HTML. User text is rendered once and escaped by
 * React, so braces or markup in a value cannot create another interpolation. */
export function rich(source: string, ...values: readonly ReactNode[]): ReactNode {
  return t(source).split(/(\{\d+\})/g).map((part, index) => {
    const slot = /^\{(\d+)\}$/.exec(part);
    const value = slot && Number(slot[1]) < values.length ? values[Number(slot[1])] : part;
    return <Fragment key={index}>{value}</Fragment>;
  });
}
