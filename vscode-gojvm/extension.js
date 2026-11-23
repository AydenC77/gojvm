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

async function activate(context) {
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
