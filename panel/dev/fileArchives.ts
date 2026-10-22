import { gunzipSync, inflateRawSync } from "node:zlib";

export interface ArchiveFile { path: string; data: Buffer; directory?: boolean }

function crc32(data: Buffer): number {
  let crc = 0xffffffff;
  for (const byte of data) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ ((crc & 1) ? 0xedb88320 : 0);
  }
  return (crc ^ 0xffffffff) >>> 0;
}

export function mockZip(files: ArchiveFile[]): Buffer {
  const local: Buffer[] = [];
  const central: Buffer[] = [];
  let offset = 0;
  for (const file of files) {
    const name = Buffer.from(file.path + (file.directory && !file.path.endsWith("/") ? "/" : ""));
    const checksum = crc32(file.data);
    const header = Buffer.alloc(30);
    header.writeUInt32LE(0x04034b50); header.writeUInt16LE(20, 4); header.writeUInt16LE(0x800, 6);
    header.writeUInt32LE(checksum, 14); header.writeUInt32LE(file.data.length, 18); header.writeUInt32LE(file.data.length, 22); header.writeUInt16LE(name.length, 26);
    local.push(header, name, file.data);
    const record = Buffer.alloc(46);
    record.writeUInt32LE(0x02014b50); record.writeUInt16LE(20, 4); record.writeUInt16LE(20, 6); record.writeUInt16LE(0x800, 8);
    record.writeUInt32LE(checksum, 16); record.writeUInt32LE(file.data.length, 20); record.writeUInt32LE(file.data.length, 24);
    record.writeUInt16LE(name.length, 28); record.writeUInt32LE(offset, 42);
    central.push(record, name);
    offset += header.length + name.length + file.data.length;
  }
  const index = Buffer.concat(central);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50); end.writeUInt16LE(files.length, 8); end.writeUInt16LE(files.length, 10);
  end.writeUInt32LE(index.length, 12); end.writeUInt32LE(offset, 16);
  return Buffer.concat([...local, index, end]);
}

export function mockArchive(data: Buffer, name: string): ArchiveFile[] {
  const files: ArchiveFile[] = [];
  if (/\.zip$/i.test(name)) {
    let end = data.length - 22;
    while (end >= 0 && data.readUInt32LE(end) !== 0x06054b50) end--;
    if (end < 0) throw new Error("archive_invalid");
    let at = data.readUInt32LE(end + 16);
    const count = data.readUInt16LE(end + 10);
    for (let i = 0; i < count; i++) {
      if (data.readUInt32LE(at) !== 0x02014b50) throw new Error("archive_invalid");
      const size = data.readUInt32LE(at + 20);
      const method = data.readUInt16LE(at + 10);
      const length = data.readUInt16LE(at + 28);
      const path = data.subarray(at + 46, at + 46 + length).toString();
      const local = data.readUInt32LE(at + 42);
      const start = local + 30 + data.readUInt16LE(local + 26) + data.readUInt16LE(local + 28);
      const compressed = data.subarray(start, start + size);
      const body = method === 0 ? compressed : method === 8 ? inflateRawSync(compressed, { maxOutputLength: 64 << 20 }) : null;
      if (!body || body.length !== data.readUInt32LE(at + 24) || crc32(body) !== data.readUInt32LE(at + 16)) throw new Error("archive_invalid");
      if (((data.readUInt32LE(at + 38) >>> 16) & 0xf000) === 0xa000) throw new Error("archive_symlink");
      files.push({ path, data: body, directory: path.endsWith("/") });
      at += 46 + length + data.readUInt16LE(at + 30) + data.readUInt16LE(at + 32);
    }
    return files;
  }
  if (/\.(tgz|tar\.gz)$/i.test(name)) data = gunzipSync(data, { maxOutputLength: 64 << 20 });
  else if (!/\.tar$/i.test(name)) throw new Error("archive_invalid");
  for (let at = 0; at + 512 <= data.length && data[at] !== 0;) {
    const text = (start: number, size: number) => data.subarray(at + start, at + start + size).toString().replace(/\0.*$/s, "");
    const path = [text(345, 155), text(0, 100)].filter(Boolean).join("/");
    const size = parseInt(text(124, 12).trim(), 8) || 0;
    const type = text(156, 1);
    if (["1", "2"].includes(type)) throw new Error("archive_symlink");
    if (!["", "0", "5"].includes(type)) throw new Error("archive_unsafe");
    if (at + 512 + size > data.length) throw new Error("archive_invalid");
    files.push({ path, data: data.subarray(at + 512, at + 512 + size), directory: type === "5" });
    at += 512 + Math.ceil(size / 512) * 512;
  }
  if (!files.length) throw new Error("archive_invalid");
  return files;
}
