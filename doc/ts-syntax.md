# TypeScript 语法笔记

读 `yk-tako` 代码时碰到的语法，按出现位置记。TypeScript 继承 JavaScript 的运行时语法，下面两条都是 JavaScript 写法，在 `.ts` 里同样成立。

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
