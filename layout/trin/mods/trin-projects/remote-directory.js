import { exec, readTextFile, appHome } from "herdr";

export const directoryMessage = code => ({ permission: "Нет доступа к этой директории.", not_found: "Директория или цель ссылки не найдена.",
  not_directory: "Этот путь не является директорией.", symlink_loop: "Обнаружен цикл символьных ссылок.", invalid_path: "Укажите корректный путь к директории.",
  unsupported: "Выбор директории пока поддерживает только POSIX-машины.", invalid_query: "Сократите фильтр до 1024 символов." }[code] ?? "Не удалось прочитать директорию.");

export function parseDirectoryResult(result, validate = false) {
  if (result.code !== 0) throw new Error("Не удалось прочитать директорию. Проверьте подключение и наличие python3 на машине.");
  let data;
  try { data = JSON.parse(result.stdout); } catch { throw new Error("Машина вернула некорректный список директорий."); }
  if (!data || data.ok !== true) throw new Error(directoryMessage(data?.code));
  const path = value => typeof value === "string" && value.startsWith("/") && !/[\0\r\n]/.test(value);
  if (!path(data.path) || !path(data.home) || typeof data.realPath !== "string") throw new Error("Машина вернула некорректный путь.");
  if (!validate && (!Array.isArray(data.entries) || data.entries.length > 200 || data.entries.some(e => !e || typeof e.name !== "string"
    || !e.name || e.name === "." || e.name === ".." || /[\/\0]/.test(e.name) || typeof e.link !== "boolean")
    || (data.nextCursor !== null && typeof data.nextCursor !== "string"))) throw new Error("Машина вернула некорректный список директорий.");
  return data;
}

export function createDirectoryReader() {
  let source;
  return async request => {
    if (!request.machine) throw new Error("Не выбрана удалённая машина.");
    source ??= readTextFile(`${appHome()}/mods/trin-projects/remote-directory.py`);
    const script = await source;
    if (!script) throw new Error("Не найден модуль чтения директорий. Перезагрузите Projects.");
    const { machine, ...payload } = request;
    const result = await exec("python3", ["-c", script, JSON.stringify(payload)], { on: machine });
    return parseDirectoryResult(result, request.validate);
  };
}
