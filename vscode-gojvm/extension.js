// gojvm VS Code extension: starts `gojvm lsp` (diagnostics, completion,
// formatting) and adds run/build/check commands.
const vscode = require('vscode');
const path = require('path');
const { LanguageClient } = require('vscode-languageclient/node');

let client;
let statusItem;
let terminal;

function binary() {
  return vscode.workspace.getConfiguration('gojvm').get('path') || 'gojvm';
}

function quote(p) {
  return /\s/.test(p) ? `"${p}"` : p;
}

async function startClient(context) {
  const exe = binary();
  client = new LanguageClient(
    'gojvm',
    'gojvm',
    { command: exe, args: ['lsp'] },
    {
      documentSelector: [{ scheme: 'file', language: 'go' }],
      synchronize: { fileEvents: vscode.workspace.createFileSystemWatcher('**/gojvm.json') },
    }
  );
  try {
    await client.start();
    statusItem.text = '$(check) gojvm';
    statusItem.tooltip = 'gojvm language server is running (click to run the current package)';
  } catch (err) {
    statusItem.text = '$(error) gojvm';
    statusItem.tooltip = 'gojvm language server failed to start';
    const pick = await vscode.window.showErrorMessage(
      `gojvm: could not start "${exe}" (${err.message || err}). Install it with: go install github.com/AydenC77/gojvm@latest`,
      'Open settings'
    );
    if (pick === 'Open settings') {
      vscode.commands.executeCommand('workbench.action.openSettings', 'gojvm.path');
    }
    client = undefined;
  }
}

async function restart(context) {
  if (client) {
    try { await client.stop(); } catch (_) { /* already stopped */ }
    client = undefined;
  }
  await startClient(context);
}

// gopls cannot resolve `import "java/util"` and would paint false errors.
async function suggestDisablingGopls() {
  const cfg = vscode.workspace.getConfiguration('gojvm');
  if (!cfg.get('suggestDisablingGopls')) return;
  const go = vscode.workspace.getConfiguration('go');
  if (go.get('useLanguageServer') === false) return;
  const pick = await vscode.window.showInformationMessage(
    'gojvm: the Go language server (gopls) cannot resolve Java imports and will show false errors in this project. Turn it off for this workspace? (gojvm provides its own completion, errors and formatting.)',
    'Turn off gopls here',
    'Not now',
    "Don't ask again"
  );
  if (pick === 'Turn off gopls here') {
    await go.update('useLanguageServer', false, vscode.ConfigurationTarget.Workspace);
    vscode.window.showInformationMessage('gopls disabled for this workspace. Reload the window to apply.', 'Reload')
      .then((p) => p === 'Reload' && vscode.commands.executeCommand('workbench.action.reloadWindow'));
  } else if (pick === "Don't ask again") {
    await cfg.update('suggestDisablingGopls', false, vscode.ConfigurationTarget.Global);
  }
}

function targetDir() {
  const ed = vscode.window.activeTextEditor;
  if (ed && ed.document.uri.scheme === 'file') return path.dirname(ed.document.uri.fsPath);
  const f = vscode.workspace.workspaceFolders;
  return f && f.length ? f[0].uri.fsPath : undefined;
}

function runInTerminal(sub) {
  const dir = targetDir();
  if (!dir) {
    vscode.window.showWarningMessage('gojvm: open a folder first.');
    return;
  }
  vscode.workspace.saveAll(false).then(() => {
    if (!terminal || terminal.exitStatus !== undefined) {
      terminal = vscode.window.createTerminal({ name: 'gojvm' });
    }
    terminal.show(true);
    terminal.sendText(`cd ${quote(dir)}`);
    terminal.sendText(`${quote(binary())} ${sub}`);
  });
}

async function showDoc() {
  const target = await vscode.window.showInputBox({
    prompt: 'Java package or class (e.g. java/util or java/util/ArrayList)',
    placeHolder: 'java/util/ArrayList',
  });
  if (!target) return;
  const dir = targetDir();
  const { execFile } = require('child_process');
  execFile(binary(), ['doc', target], { cwd: dir, maxBuffer: 16 * 1024 * 1024 }, async (err, stdout, stderr) => {
    if (err && !stdout) {
      vscode.window.showErrorMessage(`gojvm doc: ${stderr || err.message}`);
      return;
    }
    const doc = await vscode.workspace.openTextDocument({ content: stdout, language: 'go' });
    vscode.window.showTextDocument(doc, { preview: true });
  });
}

// --- project wizards -------------------------------------------------------

function exec(args, cwd) {
  const { execFile } = require('child_process');
  return new Promise((resolve, reject) => {
    execFile(binary(), args, { cwd, maxBuffer: 64 * 1024 * 1024, timeout: 15 * 60 * 1000 }, (err, stdout, stderr) => {
      if (err) {
        const e = new Error((stderr || '').trim() || err.message);
        e.stdout = stdout;
        reject(e);
      } else {
        resolve(stdout);
      }
    });
  });
}

function missingBinary(err) {
  return vscode.window.showErrorMessage(
    `gojvm: ${err.message}. Is gojvm installed? (go install github.com/AydenC77/gojvm@latest)`);
}

async function askProperty(p, index, total) {
  const title = `New project (${index}/${total}) — ${p.label || p.name}`;
  if (p.type === 'bool') {
    const pick = await vscode.window.showQuickPick(['Yes', 'No'], { title, placeHolder: p.help || p.label });
    return pick === undefined ? undefined : (pick === 'Yes' ? 'true' : 'false');
  }
  if (p.type === 'select' && p.options && p.options.length) {
    const items = p.options.map((o, i) => ({ label: o, description: i === 0 ? 'latest' : undefined }));
    const pick = await vscode.window.showQuickPick(items, { title, placeHolder: p.help || p.label });
    return pick === undefined ? undefined : pick.label;
  }
  return vscode.window.showInputBox({ title, prompt: p.help || undefined, value: p.default || '', ignoreFocusOut: true });
}

async function pickParentFolder() {
  const picked = await vscode.window.showOpenDialog({
    canSelectFolders: true, canSelectFiles: false, canSelectMany: false,
    openLabel: 'Create project here', title: 'Choose where to create the project',
  });
  return picked && picked[0] ? picked[0].fsPath : undefined;
}

async function offerOpen(dir) {
  const pick = await vscode.window.showInformationMessage(
    `Project created in ${dir}`, 'Open', 'Open in new window');
  if (pick) {
    vscode.commands.executeCommand('vscode.openFolder', vscode.Uri.file(dir), { forceNewWindow: pick !== 'Open' });
  }
}

async function newProject() {
  let templates;
  try {
    templates = JSON.parse(await exec(['new', '--list', '--json']));
  } catch (err) {
    return missingBinary(err);
  }
  const pick = await vscode.window.showQuickPick(
    templates.map((t) => ({ label: t.name, description: t.group, detail: t.description, id: t.id })),
    { title: 'gojvm: New Project', placeHolder: 'Choose a template', matchOnDetail: true });
  if (!pick) return;

  let manifest;
  try {
    manifest = await vscode.window.withProgress(
      { location: vscode.ProgressLocation.Notification, title: 'gojvm: looking up versions…' },
      async () => JSON.parse(await exec(['new', '-t', pick.id, '--describe', '--json'])));
  } catch (err) {
    return vscode.window.showErrorMessage(`gojvm: ${err.message}`);
  }

  const answers = {};
  const props = manifest.properties || [];
  for (let i = 0; i < props.length; i++) {
    const v = await askProperty(props[i], i + 1, props.length);
    if (v === undefined) return; // cancelled
    answers[props[i].name] = v;
  }

  const parent = await pickParentFolder();
  if (!parent) return;
  const base = (answers.PROJECT_NAME || pick.id).replace(/\s+/g, '');
  const dir = path.join(parent, base);
  if (require('fs').existsSync(dir)) {
    return vscode.window.showErrorMessage(`gojvm: ${dir} already exists.`);
  }

  const args = ['new', '-t', pick.id, dir, '--yes'];
  for (const [k, v] of Object.entries(answers)) args.push('--set', `${k}=${v}`);
  try {
    const out = await vscode.window.withProgress(
      { location: vscode.ProgressLocation.Notification, title: `gojvm: creating ${base} (downloading dependencies…)` },
      () => exec(args));
    if (/problem:/.test(out)) {
      vscode.window.showWarningMessage('gojvm: project created, but the test build reported a problem. Run "gojvm: Check" to see it.');
    }
  } catch (err) {
    return vscode.window.showErrorMessage(`gojvm: ${err.message}`);
  }
  offerOpen(dir);
}

async function initProject() {
  const name = await vscode.window.showInputBox({
    title: 'gojvm: New Basic Project', prompt: 'Project folder name', value: 'myapp', ignoreFocusOut: true,
    validateInput: (v) => (/^[\w.-]+$/.test(v || '') ? undefined : 'Use letters, digits, - _ . only'),
  });
  if (!name) return;
  const parent = await pickParentFolder();
  if (!parent) return;
  const dir = path.join(parent, name);
  try {
    await exec(['init', dir]);
  } catch (err) {
    return vscode.window.showErrorMessage(`gojvm: ${err.message}`);
  }
  offerOpen(dir);
}

async function hasProject() {
  const found = await vscode.workspace.findFiles('gojvm.json', '**/node_modules/**', 1);
  return found.length > 0;
}

async function activate(context) {
  context.subscriptions.push(
    vscode.commands.registerCommand('gojvm.newProject', newProject),
    vscode.commands.registerCommand('gojvm.initProject', initProject)
  );
  // Language features only inside gojvm projects; the wizards work anywhere.
  if (!(await hasProject())) return;
  vscode.commands.executeCommand('setContext', 'gojvm.active', true);
  statusItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 0);
  statusItem.text = '$(sync~spin) gojvm';
  statusItem.command = 'gojvm.run';
  statusItem.show();
  context.subscriptions.push(
    statusItem,
    vscode.commands.registerCommand('gojvm.run', () => runInTerminal('run')),
    vscode.commands.registerCommand('gojvm.build', () => runInTerminal('build')),
    vscode.commands.registerCommand('gojvm.check', () => runInTerminal('check')),
    vscode.commands.registerCommand('gojvm.doc', showDoc),
    vscode.commands.registerCommand('gojvm.restart', () => restart(context)),
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('gojvm.path')) restart(context);
    })
  );
  const watcher = vscode.workspace.createFileSystemWatcher('**/gojvm.json');
  watcher.onDidChange(() => restart(context));
  watcher.onDidCreate(() => restart(context));
  context.subscriptions.push(watcher);
  await startClient(context);
  suggestDisablingGopls();
}

async function deactivate() {
  if (client) await client.stop();
}

module.exports = { activate, deactivate };
