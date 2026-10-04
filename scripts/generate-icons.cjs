#!/usr/bin/env node
// Requires sharp. On macOS, iconutil also exports the ICNS from the iconset.
const fs = require("node:fs/promises");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
const sharp = require("sharp");

const root = path.resolve(__dirname, "..");
const assets = path.join(root, "docs/assets");
const kit = path.join(assets, "icons");
const web = path.join(root, "panel/public");
const sizes = [16, 20, 24, 32, 40, 48, 64, 96, 128, 180, 192, 256, 512, 1024];

function svg(defs, content, viewBox = "0 0 1024 1024", width = 1024, height = width) {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="${viewBox}" role="img" aria-label="Felis">\n${defs}\n${content}\n</svg>\n`;
}

function place(art, width = 896) {
  const scale = width / 1040;
  return `<g transform="translate(${(1024 - width) / 2} ${(1024 - 640 * scale) / 2}) scale(${scale}) translate(-110 -445)">${art}</g>`;
}

async function render(source, size, destination, background) {
  let image = sharp(Buffer.from(source), { density: 192 }).resize(size, size);
  if (background) image = image.flatten({ background });
  const png = await image.png().toBuffer();
  await fs.writeFile(destination, png);
  return png;
}

async function main() {
  await Promise.all([kit, `${kit}/png`, `${kit}/felis.iconset`, `${web}/icons`].map((dir) => fs.mkdir(dir, { recursive: true })));
  const source = await fs.readFile(`${assets}/felis-logo.svg`, "utf8");
  const defs = source.match(/<defs>[\s\S]*?<\/defs>/)[0];
  const art = source.match(/<g id="felis">[\s\S]*<\/g>/)[0];
  const icon = svg(defs, place(art));
  const tile = '<rect width="1024" height="1024" rx="224" fill="#e9f1e3"/>';
  const app = svg(defs, tile + place(art));
  const dark = svg(defs, tile.replace("#e9f1e3", "#253d31") + place(art));
  // The entire cat fits inside the central 80% diameter safe circle.
  const maskable = svg(defs, '<rect width="1024" height="1024" fill="#e9f1e3"/>' + place(art, 680));
  const reference = svg(defs, '<rect width="1254" height="1254" fill="#fff"/>' + art, "0 0 1254 1254", 1254);
  // White unions the original filled shapes; black knocks out facial features.
  const silhouette = [...art.matchAll(/<path[^>]*\sd="([^"]+)"[^>]*\/>/g)]
    .map((match) => `<path fill="#fff" d="${match[1]}"/>`).join("");
  const cutouts = [...art.matchAll(/<(?:ellipse|path)\b[^>]*fill="url\(#eye\)"[^>]*\/>/g)]
    .map((match) => match[0].replace('fill="url(#eye)"', 'fill="#000"')).join("");
  const monoDefs = `<defs><mask id="shape" maskUnits="userSpaceOnUse" x="110" y="445" width="1040" height="640">${silhouette}${cutouts}</mask></defs>`;
  const monoArt = '<rect x="110" y="445" width="1040" height="640" fill="#000" mask="url(#shape)"/>';
  const mono = svg(monoDefs, monoArt, "110 445 1040 640", 1040, 640);
  const mask = svg(monoDefs, place(monoArt));
  for (const [name, value] of Object.entries({
    "felis-icon.svg": icon, "felis-app.svg": app, "felis-app-dark.svg": dark,
    "felis-maskable.svg": maskable, "felis-monochrome.svg": mono,
    "felis-mask.svg": mask, "felis-reference.svg": reference,
  })) await fs.writeFile(`${kit}/${name}`, value);

  const pngs = new Map();
  for (const size of sizes) pngs.set(size, await render(icon, size, `${kit}/png/icon-${size}.png`));
  await render(app, 1024, `${kit}/felis-app.png`);
  await render(dark, 1024, `${kit}/felis-app-dark.png`);
  await render(maskable, 512, `${kit}/felis-maskable.png`);
  await render(reference, 1254, `${kit}/felis-reference.png`);
  await sharp(Buffer.from(source), { density: 192 }).resize(540, 341, { fit: "contain", background: "#00000000" }).png().toFile(`${assets}/felis-logo.png`);

  const icoSizes = [16, 24, 32, 48, 64, 128, 256];
  const directory = Buffer.alloc(6 + 16 * icoSizes.length);
  directory.writeUInt16LE(1, 2);
  directory.writeUInt16LE(icoSizes.length, 4);
  let offset = directory.length;
  icoSizes.forEach((size, index) => {
    const data = pngs.get(size);
    const entry = 6 + index * 16;
    directory[entry] = directory[entry + 1] = size === 256 ? 0 : size;
    directory.writeUInt16LE(1, entry + 4);
    directory.writeUInt16LE(32, entry + 6);
    directory.writeUInt32LE(data.length, entry + 8);
    directory.writeUInt32LE(offset, entry + 12);
    offset += data.length;
  });
  await fs.writeFile(`${kit}/felis.ico`, Buffer.concat([directory, ...icoSizes.map((size) => pngs.get(size))]));

  for (const size of [16, 32, 128, 256, 512]) {
    await render(app, size, `${kit}/felis.iconset/icon_${size}x${size}.png`);
    await render(app, size * 2, `${kit}/felis.iconset/icon_${size}x${size}@2x.png`);
  }
  if (process.platform === "darwin") execFileSync("iconutil", ["-c", "icns", `${kit}/felis.iconset`, "-o", `${kit}/felis.icns`]);

  await fs.writeFile(`${web}/favicon.svg`, icon);
  await fs.copyFile(`${kit}/felis.ico`, `${web}/favicon.ico`);
  for (const size of [16, 32]) await fs.writeFile(`${web}/favicon-${size}x${size}.png`, pngs.get(size));
  await fs.writeFile(`${web}/safari-pinned-tab.svg`, mask);
  await render(app, 180, `${web}/apple-touch-icon.png`, "#e9f1e3");
  for (const size of [192, 512]) {
    await render(app, size, `${web}/icons/icon-${size}.png`);
    await render(maskable, size, `${web}/icons/icon-maskable-${size}.png`, "#e9f1e3");
  }

  const background = Buffer.from(`<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="1000">
    <rect width="1200" height="1000" fill="#f7f9f5"/>
    <g font-family="Arial, sans-serif" fill="#253d31">
      <text x="48" y="64" font-size="30" font-weight="700">Felis / Icon Kit</text>
      <text x="48" y="99" font-size="16" fill="#60745e">SVG paths · transparent PNG · ICO · ICNS · web icons</text>
      <rect x="48" y="128" width="700" height="475" rx="24" fill="#fff"/>
      <text x="76" y="166" font-size="15">VECTOR TRACE</text>
      <rect x="776" y="128" width="376" height="475" rx="24" fill="#253d31"/>
      <text x="804" y="166" font-size="15" fill="#e9f1e3">ON DARK</text>
      <text x="48" y="654" font-size="15">APP / LIGHT</text>
      <text x="294" y="654" font-size="15">APP / DARK</text>
      <text x="540" y="654" font-size="15">MASKABLE</text>
      <text x="786" y="654" font-size="15">MONOCHROME</text>
      <text x="48" y="956" font-size="15" fill="#60745e">16 / 24 / 32 / 48 / 64 / 128 px</text>
    </g></svg>`);
  const large = await sharp(Buffer.from(source), { density: 192 }).resize(644, 396).png().toBuffer();
  const onDark = await sharp(Buffer.from(source), { density: 192 }).resize(328, 202).png().toBuffer();
  const previews = await Promise.all([app, dark, maskable, mask].map((value) => sharp(Buffer.from(value), { density: 192 }).resize(210, 210).png().toBuffer()));
  const layers = [{ input: large, left: 76, top: 184 }, { input: onDark, left: 800, top: 277 },
    ...previews.map((input, index) => ({ input, left: 48 + index * 246, top: 682 }))];
  let left = 420;
  for (const size of [16, 24, 32, 48, 64, 128]) {
    layers.push({ input: pngs.get(size), left, top: 978 - size });
    left += size + 32;
  }
  await sharp(background).composite(layers).png().toFile(`${kit}/preview.png`);
  console.log(`Generated Felis icons in ${kit} and ${web}`);
}

main().catch((error) => { console.error(error); process.exitCode = 1; });
