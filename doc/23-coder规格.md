# 23 · coder 规格

> 性质：讨论稿，未定案。2026-10-12。
>
> 点头之前不写 `tmp/` 台账、不改代码。本篇不加端点、不加帧 kind、不加表。
>
> 定案前不改 `doc/16` 的索引。
>
> `doc/17` 第 3.1 节把 `agents/coder.md` 写成 bash 之后的新文件，并给过六项名单。本篇取代那六项。施工仍排在 bash 已经进父池之后。

## 1. 要解决的那一次委派

用户把 workspace 里 40 个日志交给 tako：聚合成出现最多的 10 种错误，脚本留在 workspace，回复里给路径和前 10 名。

bash 进父池、并且 `researcher.md` 写上不含 `bash` 的白名单之后，这条链是断的。主代理自己拿得到 `bash`。它若把 40 个文件读进主线程再跑，日志正文留在主线程的 `tool_result` 里，后面每一轮都还在主上下文里。`researcher` 拿不到 `bash`。它能 `write_file` 写出 `agg.py`，跑不了。终稿里只能写「脚本已写好、没执行」。没有第三个角色同时满足两件事：自己的上下文里能跑命令，交回主线程的只有路径和结论。

`coder` 就是这个角色。它是 `agents/coder.md` 装出来的子代理，主线程把它当一个工具来调。工具名是 frontmatter 的 `name`，也就是 `coder`。主线程收到的是终稿，超过 50 KB 仍按现有委派帽截断。40 个日志的正文留在它自己的消息里。

链路：

```
用户要求聚合 40 个日志
  → 主代理调用 coder（任务文字只描述要什么，不把 40 份正文抄进参数）
  → coder 用 list_directory / search_files 找到日志
  → write_file 写下 agg.py
  → bash 在 workspace 根执行
  → 终稿只写脚本路径、结果路径、前 10 名
  → 主线程的 tool_result 是这份终稿
```

本篇插在「主代理决定委派给谁」这一环。上游是用户的这条消息。下游是主线程拿到终稿。中间缺的是一个拿得到 `bash`、并且被正文约束成「不要把过程交回来」的子代理。

## 2. 文件

只新增 `yk-tako-eino/agents/coder.md`。装载代码不改。今天 `BeforeAgent` 每场 run 起手扫 `agents/*.md`，按文件名排序。`coder.md` 和 `researcher.md` 各装一条。`name` 撞车时，文件名排序在后的那份拒载，所以 `name` 用 `coder`，不复用 `researcher`。

`model` 这一行不写。缺席等于继承主模型，和 `researcher.md` 今天一样。不给 coder 单独配一家模型。

`tools` 必须写。这一行缺席是空指针，子代理拿到父池的一份拷贝。父池里现在有 `search_history` 的真查询。`doc/21` 第 3.4 节的拒绝壳还没落地（台账 H3 未做），壳又写在「整池拷贝」那条路上。白名单按名字从父池把工具对象挑出来，挑到的是同一个对象。名单里一旦写上 `search_history`，coder 就能读主会话已经折掉的全文，再写进终稿，主线程的隔离就破了。所以这份规格不能靠继承，也不能把这个名字写进去。

全文如下。点头后按这版落盘，不在施工时再改措辞。

```markdown
---
name: coder
description: 把一段要在 workspace 里改文件、写脚本、跑命令才能完成的活整包交给它。给一段任务描述，它在自己的上下文里做完，最后带回路径和结论。主线程只收这份终稿。适合批量处理、生成物、要跑一遍才知道结果的任务。查资料、写报告不要交给它。
---

你是 tako 的子代理「coder」：接受一段要在 workspace 里动手的任务，独立做完，回传一份终稿。

工作方式：
- 批量活先写成脚本，再用 bash 跑。不要把几十个文件逐个读进上下文里数。
- 脚本和结果用 write_file 或 edit_file 写进 workspace。不要用 bash 的 heredoc 或 python 在命令字符串里直接写文件。
- 终稿只写路径和结论。主线程的上下文里只会出现这份终稿，不会出现你的命令输出和文件正文。命令输出很长时，把要留下的内容写进文件，终稿里给路径。
- 系统提示末尾的笔记是已经生效的规矩。那里写了只改某个文件、不要新建，就去改那个文件。
- 手里没有联网检索，也不能查本会话更早的历史原文。任务需要外部事实或更早一轮的原句、而终稿里写不清时，写明「未能查证」或「原文不在本轮」，不要编。

边界：只做委派任务本身。命令在 workspace 根下执行。超时、输出被截断，都照实写进终稿，不要把截断说成已经跑完。
```

`description` 是主代理决定委不委派的那一段。它写明「查资料、写报告不要交给它」，把 `researcher` 的活留在 `researcher`。本篇不改 `researcher.md` 的 description，也不改它的正文。

## 3. 白名单

名字按父池里工具声明的 `Name` 写，不是文件名。顺序不参与装载。有一个名字父池里没有，整条规格拒载，日志一行，这场 run 照常跑，只是没有 `coder` 这个工具。`researcher` 不受影响。

```yaml
tools:
  - write_file
  - read_text_file
  - edit_file
  - list_directory
  - search_files
  - memory
  - bash
```

这七个名字相对 `doc/17` 第 3.1 节的六项，改了两处：读工具用 `read_text_file`，不加 `read_file`；加上 `memory`。

`read_file` 和 `read_text_file` 是同一个函数的两个名字，参数和行为相同，超过 200 KB 且不带 `head` 或 `tail` 都会拒绝。两把都挂上，模型会各试一次，多一轮往返，读到的是同一份正文。名单只留标准名 `read_text_file`。`read_file` 仍在父池里，主代理和将来写了它的规格还能用。

`memory` 不决定它看不看得到笔记。装载时系统提示已经是「规格正文 + 空行 + 起手快照」，和工具名单无关。名单里没有 `memory`，coder 仍然读得到「deck 只改这个文件、不要新建」。它写不回新规矩：这一场发现该记的一句，终稿里可以写，`notes` 表没有新行，下一场两边的快照里没有。`doc/17` 第 7.1 节说新规格的白名单要写上 `memory` 才看得见，看得见的是这把工具。本篇把它放进名单。

每一把在那次聚合里干什么：

- `list_directory`、`search_files`：找出 40 个日志在哪。不给这两把，coder 只能 `bash ls`。`bash` 的单路输出帽把大目录截断之后，后半个目录它看不见，聚合会缺文件。
- `read_text_file`：改已经存在的脚本或结果文件之前先看见原文。不读就 `edit_file`，`old_text` 对不上，工具返回错误。
- `write_file`：把 `agg.py` 和结果文件写进 workspace。用 `bash` 的 heredoc 或 `python -c` 在脚本里 `open` 出来的路径，折叠时的 command 扫描看不见。扫不到的路径不会出现在摘要末尾那段程序拼上去的路径清单里，下一场模型会以为文件不存在、再写一份。`write_file` 的 `file_effect` 是 `write`，路径会进这段清单。
- `edit_file`：笔记要求只改已有文件时，换其中一段，而不是整文件覆盖。覆盖同一路径不算「新建」，但没读全的后半段会丢。
- `bash`：跑 `python3 agg.py`。没有它，规格就退回「写得出、跑不了」。
- `memory`：上一节说的写回。

明确不放的，以及放进去会怎样：

- `search_history`：不放。理由在第 2 节。缺的原文让它写进终稿，主代理自己查。
- `web_search`：不放。`doc/22` 要求 coder 没有搜索。父池以后加上这个名字，白名单不写就拿不到。需要公开网页的活交给 `researcher`。
- `get_time`、`move_file`、`create_directory`、`directory_tree`、`get_file_info`、`read_media_file`、`read_file`：不放。建目录、改名可以用 `bash`，而且 `mkdir`、`mv` 在 command 扫描的写命令表里，路径仍能钉上。读图、看目录树不是这次聚合要的动作。多一把，模型就多一条可以试的旁路。

`bash` 还不在父池时，这份文件一装上去就会因未知名 `bash` 整条拒载。所以本篇不和 bash 那一拍绑在同一次改动里验收。bash 那一拍负责把工具放进父池，并给 `researcher` 写上没有 `bash` 的白名单。本篇只在那之后加这一个文件。

## 4. 和已经落地的几处怎么接

快照。不新写读法。`coder` 和 `researcher` 共用装载时读到的那一份。同一场里 coder 调用 `memory` 写下的新行，主代理这场的快照看不见，下一场才进入两边。这是笔记层已经定下的时点，本篇不改。

委派帽。终稿超过 50 KB 仍截断。正文要求只交路径和结论，就是为了终稿本身不要顶到这顶帽。帽的数字本篇不动。

子代理自己的窗口。连着读、连着跑，正文堆在 coder 自己的消息里，终稿还没交出来窗口也可能满。那是 `doc/17` 第 6 节的事。本篇不给子代理挂折叠。`bash` 单路输出有帽，一次调用的结果有界；很多轮叠在一起仍会满。正文让它把长输出写进文件，是在折叠落地之前少往窗口里堆，不是折叠的替代。

四个 API 的路径和字节不变。调用走现有的 `tool_call` / `tool_result`。子代理内部那几轮的帧带 `agent=coder`，和今天 `researcher` 的帧一样。

## 5. 本篇不做

不改 `researcher.md`。不改父池的装配顺序。不实现 `bash`，不实现手动停止，不实现 `web_search`，不实现子代理折叠。

不写危险命令规则表，不补符号链接逃出 workspace，不禁止 `bash` 访问网络。这三件 `doc/17` 第 3 节已经登记在 bash 那一拍之外。

不新增第二种回传格式。主线程看见的仍是一段终稿文本。

不把 `coder` 设成默认承接一切多步任务的角色。`description` 把调研留在 `researcher`。主代理把聚合脚本委派给 `researcher` 时，`researcher` 没有 `bash`，终稿会写做不了。验收不靠模型自己猜该找谁，任务文字写明 `coder`。

## 6. 验收

引擎按现有方式启动。先 `lsof` 看端口，被占就换，不杀别人的进程。Redis 与 PostgreSQL 用开发库现端口。这一拍开始时，父池里已经有 `bash`，`researcher.md` 的白名单里没有 `bash`。

```bash
curl -sS -X POST localhost:<port>/api/runs \
  -H 'content-type: application/json' \
  -d '{"messageId":"m_coder","text":"请委派 coder 完成这件事，不要自己做：在 workspace 里用 write_file 写 agg.py，内容是 print(424242)。然后用 bash 执行 python3 agg.py。终稿只写脚本路径和输出里的数字。"}'
```

frames 里有主线程的 `tool_call`，`name` 为 `coder`。随后有 `agent` 为 `coder` 的帧：一条 `name` 为 `write_file`，一条 `name` 为 `bash`。`bash` 那条 `tool_result` 含 `424242` 和退出码 0。主线程 `name=coder` 的 `tool_result` 含 `agg.py` 和 `424242`，并且不是 `agg.py` 的源码全文。`terminal` 的 `status` 是 `complete`。同一场 frames 里，`agent` 为 `coder` 的工具名里没有 `search_history`，也没有 `web_search`。

笔记面。先在一场 run 里用 `memory` 记下「结果只写到 output/agg.txt，不要新建别的结果文件」，`quote` 用用户原句。下一场任务文字不出现这条路径，并写明委派 `coder` 去跑上一场留下的 `agg.py`，把数字写进笔记指定的那个文件。引擎日志 `agent system coder:` 那一行含 `output/agg.txt`。`agent` 为 `coder` 的 `write_file` 或 `edit_file`，参数里的路径是 `output/agg.txt`。没有另一条结果文件的 `write_file`。

四端点的路径和字段面与这一拍之前同形。`go test ./internal/runengine/ -count=1` 全绿。本篇不改 Go，这组测试是为了证明没有旁路改动。

`bash` 未进父池时的拒载，不另开活体。`resolveToolPool` 对未知名已经有测试。本篇的文件在那个前提下会整条拒载，日志含 `bash`。

## 7. 所以这解决了什么

40 个日志的聚合有一个子代理能写脚本、能执行、并把路径和结论交回主线程。日志正文和命令的长输出留在这个子代理的消息里。`researcher` 仍然没有 `bash`。coder 读得到笔记快照，也能把新规矩写回去。它查不到本会话更早的历史原文，也没有联网检索。

`doc/17` 第 3.1 节原来的六项名单是 `write_file`、`edit_file`、`read_file`、`list_directory`、`search_files`、`bash`。本篇改成第 3 节这七项。
