// node-sealed-v1: cooperative, self-contained unit tests. Not a hostile-code
// sandbox. Node's permission model supplies a second filesystem/process check.
import { registerHooks, builtinModules } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import path from 'node:path';

const root = path.resolve(process.cwd());
const reporter = pathToFileURL(path.join(path.dirname(fileURLToPath(import.meta.url)), 'reporter.cjs')).href;
const allowed = new Set(['test', 'assert', 'assert/strict', 'timers', 'timers/promises']);
const builtins = new Set(builtinModules.map(name => name.replace(/^node:/, '')));
const denied = name => { throw new Error(`node-sealed-v1: undeclared input or capability: ${name}`); };
const builtin = name => {
  const plain = name.replace(/^node:/, '');
  if (!allowed.has(plain)) denied(name);
};
registerHooks({
  resolve(specifier, context, nextResolve) {
    // Reporter is trusted runtime code, outside the sealed project. Tests may
    // neither import it nor grant themselves its capability set.
    if (context.parentURL === reporter) return nextResolve(specifier, context);
    if (specifier.startsWith('node:') || builtins.has(specifier)) {
      builtin(specifier);
      return nextResolve(specifier, context);
    }
    if (!specifier.startsWith('.') && !specifier.startsWith('file:') && !path.isAbsolute(specifier)) denied(specifier);
    const result = nextResolve(specifier, context);
    if (!result.url.startsWith('file:')) denied(result.url);
    if (result.url === reporter && !context.parentURL) return result;
    const target = fileURLToPath(result.url);
    const relative = path.relative(root, target);
    if (relative.startsWith('..' + path.sep) || relative === '..' || path.isAbsolute(relative) || relative.split(path.sep).includes('node_modules')) denied(specifier);
    // .js type detection can consult an ancestor package.json outside the
    // sealed root. Explicit CJS/ESM extensions avoid that undeclared input.
    if (!['.cjs', '.mjs', '.json'].includes(path.extname(target))) denied(specifier);
    return result;
  },
});
const getBuiltinModule = process.getBuiltinModule.bind(process);
process.getBuiltinModule = name => { builtin(name); return getBuiltinModule(name); };
process.binding = name => denied(`process.binding(${name})`);
process._linkedBinding = name => denied(`process._linkedBinding(${name})`);
globalThis.fetch = () => denied('fetch');
globalThis.WebSocket = class { constructor() { denied('WebSocket'); } };
