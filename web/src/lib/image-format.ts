type ImageSource = { b64_json?: string; url?: string; image_rel?: string };

export function imageMimeType(image: ImageSource): string {
  const base64 = image.b64_json || "";
  if (base64.startsWith("iVBORw0KGgo")) return "image/png";
  if (base64.startsWith("R0lGOD")) return "image/gif";
  if (base64.startsWith("/9j/")) return "image/jpeg";
  if (base64.startsWith("UklGR")) return "image/webp";
  const dataMime = /^data:(image\/(?:png|gif|jpeg|webp))[;,]/i.exec(image.url || "");
  if (dataMime) return dataMime[1].toLowerCase();
  const path = (image.image_rel || image.url || "").split(/[?#]/, 1)[0].toLowerCase();
  if (path.endsWith(".gif")) return "image/gif";
  if (/\.jpe?g$/.test(path)) return "image/jpeg";
  if (path.endsWith(".webp")) return "image/webp";
  return "image/png";
}

export function imageFileExtension(mime: string): string {
  switch (mime.split(";", 1)[0]) {
    case "image/gif": return "gif";
    case "image/jpeg": return "jpg";
    case "image/webp": return "webp";
    default: return "png";
  }
}
