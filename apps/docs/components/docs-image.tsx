import type { ComponentProps } from "react";
import { ImageZoom } from "fumadocs-ui/components/image-zoom";

export function DocsImage({ src, alt = "", ...props }: ComponentProps<"img">) {
  if (typeof src === "object") {
    return <img {...props} src={src} alt={alt} />;
  }

  return <ImageZoom {...props} src={src} alt={alt} />;
}
