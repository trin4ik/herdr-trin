import { exec } from "herdr";

// Local Compose default. For a remote service use HTTPS (see README.md).
export const QUOTA_URL = "http://127.0.0.1:8098/v1/quotas";

export function quotaURL(value = QUOTA_URL) {
  // No URL credentials, query secrets, redirects or plaintext remote origins.
  if (typeof value !== "string" || !/^(https:\/\/[A-Za-z0-9.-]+(?::[0-9]+)?|http:\/\/127\.0\.0\.1:8098)\/v1\/quotas$/.test(value)) {
    throw new Error("Ожидается локальный quota-hub или HTTPS URL /v1/quotas без credentials/query");
  }
  return value;
}

export function parseResponse(result) {
  if (result.code !== 0) throw new Error("Сервис квот недоступен: сеть, TLS или таймаут 15 с");
  const split = result.stdout.lastIndexOf("\n");
  const status = Number(result.stdout.slice(split + 1));
  if (!Number.isInteger(status) || status < 100 || status > 599) throw new Error("Нет HTTP-статуса сервиса квот");
  if (status !== 200) throw Object.assign(new Error(`Сервис квот: HTTP ${status}`), { status });
  const body = result.stdout.slice(0, split);
  if (body.length > 1048576) throw new Error("Ответ сервиса квот слишком большой");
  try { return JSON.parse(body); } catch { throw new Error("Сервис квот вернул некорректный JSON"); }
}

export async function requestQuotas(endpoint = QUOTA_URL) {
  const url = quotaURL(endpoint);
  // The installed native fetch has no timeout/AbortSignal API. curl is bounded
  // below Herdr's 30s exec limit; no shell interpolation, auth or provider URLs.
  return parseResponse(await exec("curl", ["--disable", "--silent", "--show-error", "--connect-timeout", "5", "--max-time", "15",
    "--max-filesize", "1048576", "--proto", url.startsWith("https:") ? "=https" : "=http", "--noproxy", "127.0.0.1", "--header", "Accept: application/json",
    "--write-out", "\n%{http_code}", url]));
}
