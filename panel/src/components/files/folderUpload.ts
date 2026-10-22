interface FileEntry {
  name: string;
  isFile: boolean;
  isDirectory: boolean;
  file: (success: (file: File) => void, failure: (error: DOMException) => void) => void;
  createReader: () => { readEntries: (success: (entries: FileEntry[]) => void, failure: (error: DOMException) => void) => void };
}

export async function droppedFiles(data: DataTransfer): Promise<{ files: File[]; paths: string[] }> {
  const files: File[] = [];
  const paths: string[] = [];
  async function walk(entry: FileEntry, parent: string) {
    const path = parent ? `${parent}/${entry.name}` : entry.name;
    if (entry.isFile) {
      files.push(await new Promise<File>((resolve, reject) => entry.file(resolve, reject)));
      paths.push(path);
      return;
    }
    if (!entry.isDirectory) return;
    const reader = entry.createReader();
    for (;;) {
      const entries = await new Promise<FileEntry[]>((resolve, reject) => reader.readEntries(resolve, reject));
      if (entries.length === 0) break;
      for (const child of entries) await walk(child, path);
    }
  }
  for (const item of Array.from(data.items ?? [])) {
    if (item.kind !== "file") continue;
    const entry = item.webkitGetAsEntry?.() as FileEntry | null;
    if (entry) await walk(entry, "");
    else {
      const file = item.getAsFile();
      if (file) { files.push(file); paths.push(file.name); }
    }
  }
  if (!data.items?.length) {
    files.push(...Array.from(data.files));
    paths.push(...files.map((file) => file.name));
  }
  return { files, paths };
}
