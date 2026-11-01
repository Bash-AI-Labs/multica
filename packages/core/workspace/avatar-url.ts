import { api } from "../api";

export function resolvePublicFileUrlWithBase(rawUrl: string | null | undefined, baseUrl: string): string | null {
  if (!rawUrl) return null;
  if (!rawUrl.startsWith("/")) return rawUrl;
  let baseEnd = baseUrl.length;
  while (baseEnd > 0 && baseUrl[baseEnd - 1] === "/") baseEnd--;
  const trimmedBaseUrl = baseUrl.slice(0, baseEnd);
  return `${trimmedBaseUrl}${rawUrl}`;
}

export function resolvePublicFileUrl(rawUrl: string | null | undefined): string | null {
  return resolvePublicFileUrlWithBase(rawUrl, api.getBaseUrl());
}
