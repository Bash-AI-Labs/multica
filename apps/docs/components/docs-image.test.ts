// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { ImageZoom } from "fumadocs-ui/components/image-zoom";
import type { StaticImageData } from "next/image";
import { DocsImage } from "./docs-image";

vi.mock("fumadocs-ui/components/image-zoom", () => ({
  ImageZoom: () => null,
}));

describe("DocsImage", () => {
  it("preserves zoom, alt text and image attributes for Markdown URLs", () => {
    expect(
      DocsImage({
        src: "/docs/image.png",
        alt: "Fixture screenshot",
        width: 640,
        height: 480,
        loading: "lazy",
      }),
    ).toMatchObject({
      type: ImageZoom,
      props: {
        src: "/docs/image.png",
        alt: "Fixture screenshot",
        width: 640,
        height: 480,
        loading: "lazy",
      },
    });
  });

  it("keeps React Blob image sources on the native image path", () => {
    const src = new Blob(["fixture"], { type: "image/png" });
    expect(DocsImage({ src, alt: "Blob fixture" })).toMatchObject({
      type: "img",
      props: { src, alt: "Blob fixture" },
    });
  });

  const screenshot = {
    src: "/_next/static/media/quickstart-run-result.fixture.webp",
    width: 1440,
    height: 900,
    blurDataURL: "data:image/webp;base64,fixture",
  } satisfies StaticImageData;

  it.each([screenshot, { default: screenshot }])(
    "keeps compiled Markdown static-image imports zoomable",
    (src) => {
      expect(
        DocsImage({
          src,
          alt: "Completed issue run",
          loading: "lazy",
        }),
      ).toMatchObject({
        type: ImageZoom,
        props: {
          src,
          alt: "Completed issue run",
          loading: "lazy",
        },
      });
    },
  );

  it("keeps decorative images explicitly unnamed", () => {
    expect(DocsImage({ src: "/docs/decoration.png" })).toMatchObject({
      props: { alt: "" },
    });
  });
});
