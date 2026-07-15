// OCP 4.14 互換フォーマットにパッチする
// 新SDK(4.22)が生成する形式と OCP 4.14 コンソールが期待する形式の差異を吸収する
const fs = require('fs');
const path = require('path');
const distDir = path.join(__dirname, '../dist');

// --- plugin-manifest.json ---
// 新SDK: registrationMethod/loadScripts/baseURL/buildHash/customProperties を含む
// 4.14: {name, version, displayName, description, dependencies, extensions} のみ
const manifestPath = path.join(distDir, 'plugin-manifest.json');
const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
const patched = {
  name: manifest.name,
  version: manifest.version,
  displayName: manifest.customProperties?.console?.displayName || manifest.displayName || manifest.name,
  description: manifest.customProperties?.console?.description || manifest.description || '',
  dependencies: { '@console/pluginAPI': '*' },
  extensions: manifest.extensions,
};
fs.writeFileSync(manifestPath, JSON.stringify(patched));
console.log('plugin-manifest.json patched');

// --- plugin-entry.js ---
// 新SDK: __load_plugin_entry__("name", container)  ← 4.14 コンソールに存在しない
// 4.14:  window.loadPluginEntry("name@version", container) ← kubevirt-plugin と同じ形式
const entryPath = path.join(distDir, 'plugin-entry.js');
let entry = fs.readFileSync(entryPath, 'utf8');
const pluginId = `${patched.name}@${patched.version}`;
entry = entry.replace(
  `__load_plugin_entry__("${patched.name}"`,
  `window.loadPluginEntry("${pluginId}"`
);
fs.writeFileSync(entryPath, entry);
console.log(`plugin-entry.js patched: __load_plugin_entry__ -> window.loadPluginEntry("${pluginId}", ...)`);

