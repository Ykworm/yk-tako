# TypeScript 语法笔记

读 `yk-tako` 代码时碰到的语法，按出现位置记。未特别说明的是 JavaScript 运行时语法，在 `.ts` 里同样成立。标明「仅类型」的写法只给编译器看，编译成 JavaScript 后会被擦掉。

## 参数解构 `{ expression }`

出处：`src/tools/index.ts` 的 `calculate`。

```ts
async ({ expression }) => {
  // expression 已是字符串，例如 "1234*5678"
}
```

花括号写在参数位置，表示把传入的对象拆开，只取出同名属性。上面这行等价于：

```ts
async (args) => {
  const expression = args.expression;
}
```

模型通过 function calling 传来的参数是一个对象，形状由同文件里的 Zod schema 决定：

```ts
schema: z.object({
  expression: z.string().describe("要计算的数学表达式"),
}),
```

所以回调实际收到的是 `{ expression: "1234*5678" }`。schema 里再加字段时，参数也可以一起拆出来，例如 `async ({ expression, precision }) =>`。

这和对象字面量 `{ expression }` 不是同一种花括号。字面量是在构造对象；参数位置的花括号是在拆对象。

## 严格模式 `"use strict"`

出处：同一处 `calculate` 里动态求值的那一行。

```ts
const result = Function(`"use strict"; return (${expression})`)();
```

`"use strict"` 是严格模式指令。放在脚本或函数体的最前面时，后面的代码按更严格的规则执行。它必须是第一条语句；前面若有别的语句，这行就只是一个普通字符串，不会生效。

这里 `Function(...)` 会现场生成一个函数再立刻调用。若 `expression` 是 `1+2`，生成的函数体是：

```js
"use strict"; return (1+2)
```

指令写在生成函数的开头，所以只约束这个函数，不影响外层文件。

严格模式里和这段代码相关的差异：

- 给未声明的变量赋值会抛错，不会悄悄变成全局变量。
- 普通函数调用时 `this` 是 `undefined`，不会指向全局对象。
- `eval` 里声明的变量不会泄漏到外面。

上面那行前面还有正则，只放行数字和 `+ - * / ( ) %`。正则负责拦住任意代码；`"use strict"` 是再收紧一层，避免表达式里的赋值污染外层作用域。

## 对象展开 `...config`

出处：`src/l0/02-langgraph.ts` 里调用 `agent.stream` 的第二个参数。

```ts
const config = { configurable: { thread_id: "demo-1" } };

agent.stream(
  { messages: [new HumanMessage(input)] },
  { ...config, streamMode: "updates" },
);
```

`...` 写在对象字面量里、后面跟一个对象，表示把那个对象的字段抄进新对象。上面第二个参数等价于手写：

```ts
{
  configurable: { thread_id: "demo-1" },
  streamMode: "updates",
}
```

`config` 本身不变。展开出来的是一份新对象：先放入 `config` 的字段，再写入后面显式写的字段。同名字段以后写的为准，所以这里若改成 `{ streamMode: "values", ...config }`，而 `config` 里又有 `streamMode`，就会把前面的 `"values"` 盖掉。

这和上一节的参数解构方向相反。参数位置的 `{ expression }` 是把传入的对象拆开取出字段；对象字面量里的 `...config` 是把已有对象的字段装进新对象。两个参数上的花括号都是对象字面量，不是 `new`。`new` 只出现在 `new HumanMessage(input)`。

`...` 出现在函数参数位置时是另一件事，叫剩余参数，把多出来的实参收成数组：

```ts
function log(label: string, ...rest: string[]) {
  // log("a", "b", "c") 时，rest 是 ["b", "c"]
}
```

这一处是展开已有对象，不是收集参数。

## 普通对象与 `new` 出来的实例

出处：同一处 `agent.stream`。`config` 和两个参数都是普通对象；`new HumanMessage(input)` 是类实例。

```ts
const config = { configurable: { thread_id: "demo-1" } };

agent.stream(
  { messages: [new HumanMessage(input)] },
  { ...config, streamMode: "updates" },
);
```

两种写法运行时都得到对象。差别在构造过程和原型。

对象字面量 `{ ... }` 只包含写出来的字段，原型是 `Object.prototype`。`config` 因此只有 `configurable.thread_id`。调用方按字段名读取，不依赖它身上的方法。

`new HumanMessage(input)` 会先执行构造函数：把传入的字符串放进 `content`，并补上类要求的字段，例如 `type: "human"`。实例的原型链是 `HumanMessage.prototype` → `BaseMessage.prototype`，所以带得上类里定义的方法，例如 `text`、`toDict()`、`getType()`。`instanceof HumanMessage` 沿这条原型链判断，结果为真。

手写一个字段相近的对象，对不齐方法和类型判断：

```ts
const plain = { content: input, type: "human" as const };
plain.content;                  // 有，字面量里写了
plain.toDict();                 // 没有这个方法
plain instanceof HumanMessage;  // false
```

这份代码里，`config` 和 `stream` 的参数只是一袋字段，用对象字面量。一条消息要被图当成 `HumanMessage` 使用，就用 `new` 造出带方法和类型标记的实例。

## `for...of` 与 `Object.entries`

出处：`src/l0/03-deepagents.ts` 里打印每个节点增量的内层循环。`src/l0/02-langgraph.ts` 同一位置写法相同。

```ts
for (const [node, update] of Object.entries(chunk)) {
```

`streamMode: "updates"` 吐出的 `chunk` 是「节点名 → 这次增量」：

```ts
{
  model_request: { messages: [/* 模型刚产生的消息 */] },
  tools: { messages: [/* 工具刚返回的消息 */] },
}
```

`Object.entries(chunk)` 把对象变成数组。每一项都是长度为 2 的 `[键, 值]`：

```ts
[
  ["model_request", { messages: [/* ... */] }],
  ["tools", { messages: [/* ... */] }],
]
```

`for...of` 按顺序取出每一项。`[node, update]` 是数组解构，给这一项的两个位置起名字：`node` 接到键，`update` 接到值。一项里只有这两个位置，没有第三个元素留在外面。`chunk` 有几个节点，循环就走几轮。

手写等价于：

```ts
for (const entry of Object.entries(chunk)) {
  const node = entry[0];
  const update = entry[1];
}
```

数组解构写成 `[a, b]` 时，下标 2 及以后的元素不会绑定到名字上。`Object.entries` 的每一项长度就是 2，所以这里绑完即整项。

## 类型断言 `as`

仅类型。出处：同一循环里取出 `messages`。

```ts
const msgs = (
  update as { messages?: Array<{ content: unknown; tool_calls?: Array<{ name: string }> }> }
).messages;
```

`Object.entries` 给出的值类型很宽，编译器不知道 `update` 上有 `messages`。`as { ... }` 告诉编译器：把 `update` 当成右边这种形状来用。这里的花括号是类型，运行时不会新建对象。编译后这行只剩下读取 `update.messages`。

外层圆括号把断言包成一个整体，点号再去读 `.messages`。`update` 运行时仍是原来那个对象。

## 类型里的 `?` 与 `Array<T>`

仅类型。出处：上一节断言右边的类型字面量。

`?` 写在类型的字段名后面，表示这个字段可以不存在：

- `messages?`：`update` 上可以没有 `messages`
- `tool_calls?`：一条消息可以没有工具调用

这个 `?` 只出现在类型位置。运行时的可选链是 `?.`，三元表达式是 `条件 ? 真分支 : 假分支`，三个问号各管一件事。

`Array<元素类型>` 表示由这种元素组成的数组，和 `元素类型[]` 相同。上面那条类型拆开是：

```ts
{
  messages?: Array<{
    content: unknown;
    tool_calls?: Array<{ name: string }>;
  }>;
}
```

`unknown` 表示这里先不认定 `content` 的具体类型。后面要当字符串用时，代码用 `typeof last.content === "string"` 再收窄。

## 可选链 `?.` 与 `at(-1)`

出处：紧接着取最后一条消息。`src/l0/02-langgraph.ts` 同一行写法相同。

```ts
const last = msgs?.at(-1);
```

`msgs` 是 `undefined` 或 `null` 时，`msgs?.at(-1)` 的结果是 `undefined`，不会去调用 `at`。`msgs` 是数组时，才调用 `at(-1)`。`-1` 表示从末尾数第 1 个，也就是最后一条。空数组上 `at(-1)` 同样得到 `undefined`。

`last.tool_calls?.length` 同一规则：没有 `tool_calls` 时结果是 `undefined`；有数组时读它的 `length`。`length` 为 `0` 时，后面的 `if` 条件为假。

## 逻辑非 `!` 与 `continue`

出处：判断最后一条消息在不在，再决定要不要打印。

```ts
const last = msgs?.at(-1);
if (!last) continue;
if (last.tool_calls?.length) {
  console.log(`  → 🔧 [${node}] 调用: ...`);
}
```

`!` 写在值前面，把真假反过来。`last` 是 `undefined` 时，`!last` 为真。

`continue` 结束本轮循环，直接去取 `Object.entries` 的下一项。`!last` 为真时，后面的 `console.log` 不会执行。

`last` 有值时，`!last` 为假，`continue` 不触发。执行流落到下一行 `if (last.tool_calls?.length)`。这条消息带了工具调用，就会打出 `调用:`。

## 数组方法 `some`

出处：`src/middleware/filesystemGuard.ts` 里判断路径是否在工作区内。

```ts
const inWorkspace = candidates.some((c) => c === root || c.startsWith(root + path.sep));
```

`candidates` 是候选路径数组。里面通常有两条：模型传来、经 `path.resolve` 得到的逻辑路径，以及 `realpath` 展开符号链接后的真实路径。`(c) => ...` 是交给 `some` 的回调，`c` 是当前这一项。函数体只有一个表达式时，表达式的值就是返回值，不必写 `return`。

`some` 从第一项开始调用这个函数。只要有一项让函数返回真，`some` 立刻得到 `true`，剩下的项不再看。每一项都返回假时，`some` 得到 `false`。

所以上面这行的意思是：只要其中一条在工作区内，`inWorkspace` 就是 `true`。

手写等价于：

```ts
let inWorkspace = false;
for (const c of candidates) {
  if (c === root || c.startsWith(root + path.sep)) {
    inWorkspace = true;
    break;
  }
}
```

工作区根是 `/proj`，`candidates` 是 `["/proj/leak", "/private/etc/passwd"]` 时：第一项 `/proj/leak` 以 `/proj/` 开头，回调返回真，`some` 得到 `true`。第二条已经在工作区外，这一轮不会再被检查。

同文件的 `pathMatches` 里也有一处 `some`，意思同样是候选路径里只要有一条配上这条 glob：

```ts
return pathCandidates.some((c) => re.test(c) || (prefixRe?.test(c) ?? false));
```

## 工具类型 `ReturnType<T>`

仅类型。出处：`src/middleware/toolGuard.ts` 里 `toolGuardMiddleware` 返回对象的字段标注。

```ts
middleware: ReturnType<typeof createMiddleware>;
```

`ReturnType` 是 TypeScript 自带的工具类型（utility type），不用 import，直接写。给它一个函数类型，它抽出这个函数的返回值类型。上面这行读作：`middleware` 字段的类型 = `createMiddleware` 这个函数的返回值类型。

`typeof createMiddleware` 先取这个函数值的类型（`typeof` 写在类型位置时是「取值的类型」，和运行时的 `typeof x === "string"` 不是同一回事），`ReturnType<...>` 再从中抽出返回类型。好处是不用手抄一份返回类型：`createMiddleware` 的实现改了，这里的标注自动跟着变，两处不会脱节。

它的定义在 TypeScript 源码里只有一行：

```ts
type ReturnType<T> = T extends (...args: any[]) => infer R ? R : never;
```

读法：如果 `T` 能匹配「某个函数类型」，就用 `infer` 把返回值位置推断成一个新名字 `R` 并取出；否则得到 `never`。`extends ... ? ... : ...` 是类型位置的条件表达式，和运行时的三元表达式形状相同，但判断的是类型能不能匹配。

手写等价于把返回类型抄一遍：

```ts
middleware: AgentMiddleware; // 假设 createMiddleware 返回的是这个类型
```

能写，但 `createMiddleware` 的返回类型一变，这处就要手动同步，漏了就类型报错或更糟——悄悄错着。

同族的常用内置工具类型：

| 工具类型 | 作用 |
|---|---|
| `Parameters<T>` | 抽函数的参数类型，得到一个元组 |
| `Partial<T>` / `Required<T>` | 属性全变可选 / 全变必填 |
| `Pick<T, K>` / `Omit<T, K>` | 只留 / 去掉指定属性 |
| `Record<K, V>` | 构造「键是 K、值是 V」的对象类型 |
| `Awaited<T>` | 拆掉 Promise，取出 resolve 的类型 |

读第三方库的类型定义时，这一族出现频率很高。
