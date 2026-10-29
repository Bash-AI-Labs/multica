import type { ComponentProps } from "react";
import { ImageZoom } from "fumadocs-ui/components/image-zoom";

type DocsImageProps = Omit<ComponentProps<"img">, "src"> & {
  src?: ComponentProps<typeof ImageZoom>["src"] | Blob;
};

export function DocsImage({ src, alt = "", ...props }: DocsImageProps) {
  if (src instanceof Blob) {
    return <img {...props} src={src} alt={alt} />;
  }

  return <ImageZoom {...props} src={src} alt={alt} />;
}
