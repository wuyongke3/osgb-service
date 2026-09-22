# 航飞影像 OSGB 生产服务

可容器化部署的单用户摄影测量服务。浏览器上传航飞影像 → 自动调用 COLMAP / OpenMVS / OpenSceneGraph 重建 → 输出 **Smart3D 风格的分层 PagedLOD OSGB 瓦片树**，可打包下载。

- 前端：Vue 3 + Vite + TypeScript（影像上传、生产计划、阶段时间轴、SSE 实时日志）
- 后端：Go 1.26，**仅标准库，无第三方依赖**（任务目录、进程编排、LOD 分层、回读校验）
- 计算：**全程 CPU**，不需要 GPU 或 NVIDIA Container Toolkit

---

## 快速开始

**推荐使用 Docker 部署**（本文档以下步骤均已实测验证）。若需在 Windows 本机开发调试，见 [方式二](#方式二本机运行windowslinux-开发调试)。

### 环境要求

| 项目 | 要求 |
|---|---|
| 操作系统 | Linux 服务器（推荐）或 Windows + Docker Desktop |
| Docker | Docker Engine 20.10+ 与 Docker Compose v2 |
| 内存 | **建议 16 GB 以上**（Compose 默认限制容器 12 GB） |
| 磁盘 | 影像总量 × 3 倍以上可用空间（中间产物很大） |
| CPU | 核心越多越快，线程数自动适配 |

> 一次完整重建的中间产物可达数十 GB，例如 428 张 2.76 GB 的影像会产生 800 MB 以上的 `database.db`。请确保数据盘有充足空间。

### 方式一：Docker 部署（推荐）

#### 1. 获取代码

```bash
git clone https://github.com/wuyongke3/osgb-service.git
cd osgb-service
```

#### 2. 创建数据目录

```bash
mkdir -p runtime input
```

- `runtime/` —— 持久化目录，存放上传的影像、任务日志、中间产物和交付成果
- `input/` —— 可选。若影像数据量太大不想用浏览器上传，把宿主机影像目录挂到这里（只读）

#### 3. 准备配置文件（可选）

```bash
cp .env.example .env
```

`.env` 用于覆盖默认值，**不改也能直接运行**。常用的几项：

```ini
OSGB_MEMORY_LIMIT=12g      # 容器内存上限，按服务器实际情况调整
PIPELINE_THREADS=0         # 0 = 自动（CPU 核数 -1，上限 16）
COLMAP_MATCHER=sequential  # 匹配策略，sequential 最快，适合航飞影像
```

#### 4. 构建并启动

**首次构建需要 15–30 分钟**，因为镜像会从源码编译 OpenMVS：

```bash
docker compose up -d --build
```

也可使用一键脚本（会校验 Docker 环境并强制重建）：

```bash
bash ./scripts/install-tools.sh
```

#### 5. 验证安装

```bash
# 容器应为 healthy
docker compose ps

# 健康检查，应返回 {"go":"go1.26.x","status":"ok"}
curl http://localhost:8080/health

# 确认 7 个外部工具全部就绪
curl -s http://localhost:8080/api/config | grep -o '"native_pipeline_ready":true'
```

浏览器打开 **<http://localhost:8080>**，右上角应显示“生产引擎就绪”。

#### 6. 查看日志与排查

```bash
docker compose logs -f osgb-service                    # 服务日志
docker compose exec osgb-service cat /opt/colmap-version.txt  # 容器内 COLMAP 版本
```

若 `native_pipeline_ready` 为 `false`，说明某个工具未安装成功，打开页面右上角“工具配置”可看到具体哪一个未检测到。

#### 常用运维命令

```bash
docker compose restart              # 重启服务
docker compose down                 # 停止并删除容器（runtime/ 数据保留）
docker compose up -d --build        # 代码更新后重建
docker compose logs -f --tail 100   # 跟踪最近日志
```

> **数据安全**：所有数据保存在宿主机 `./runtime` 目录，`docker compose down` 不会删除它。升级时重新 `build` 并 `up -d --force-recreate` 即可，任务与计划列表会自动恢复。

### 方式二：本机运行（Windows/Linux 开发调试）

需要自行安装 7 个外部工具：`colmap`、`InterfaceCOLMAP`、`DensifyPointCloud`、`ReconstructMesh`、`RefineMesh`、`TextureMesh`、`osgconv`。

#### 1. 安装 Go 与 Node.js

- Go 1.26 或更高：<https://go.dev/dl/>
- Node.js 20 或更高：<https://nodejs.org/>

#### 2. 安装外部工具

**Windows：**

- COLMAP：<https://github.com/colmap/colmap/releases>，下载 `colmap-x64-windows-cuda.zip`
- OpenMVS：<https://github.com/cdcseacave/openMVS/releases>，下载 `OpenMVS_Windows_x64_CUDA.zip`
- OpenSceneGraph：<https://objexx.com/OpenSceneGraph.html>，下载 `OpenSceneGraph 3.6.5 - VC2022 - 64-bit`

OpenSceneGraph 解压后确认存在 `bin\osgconv.exe`。**不要只复制单个 `osgconv.exe`**，需保留同级 DLL 与 `osgPlugins-*` 目录，否则无法读取纹理。

**Linux：**

```bash
sudo apt-get install -y colmap openscenegraph
# OpenMVS 不在 Ubuntu 24.04 的 apt 源中，需自行编译：
# https://github.com/cdcseacave/openMVS/wiki/Building
```

#### 3. 配置工具路径

```bash
cp .env.example .env
```

在 `.env` 中填写绝对路径（正斜杠、反斜杠均可）：

```ini
COLMAP_BIN=E:/colmap-x64-windows-cuda/bin/colmap.exe
OPENMVS_INTERFACE_BIN=E:/OpenMVS_Windows_x64_CUDA/InterfaceCOLMAP.exe
OPENMVS_DENSIFY_BIN=E:/OpenMVS_Windows_x64_CUDA/DensifyPointCloud.exe
OPENMVS_RECONSTRUCT_BIN=E:/OpenMVS_Windows_x64_CUDA/ReconstructMesh.exe
OPENMVS_REFINE_BIN=E:/OpenMVS_Windows_x64_CUDA/RefineMesh.exe
OPENMVS_TEXTURE_BIN=E:/OpenMVS_Windows_x64_CUDA/TextureMesh.exe
OSGCONV_BIN=E:/OpenSceneGraph-3.6.5/bin/osgconv.exe
```

> 含空格的路径请用双引号包裹。注意 `"C:\dir\"` 这种结尾反斜杠紧邻引号的写法存在歧义，请去掉结尾反斜杠或改用正斜杠。

也可以把这些目录加入系统 `PATH`，然后保持默认的命令名即可。

#### 4. 构建前端

Go 服务会直接托管 `web/` 目录，因此**必须先构建前端**：

```bash
cd frontend
npm install
npm run build      # 产物输出到 ../web
cd ..
```

#### 5. 启动服务

```bash
go run ./cmd/server
```

浏览器打开 <http://localhost:8080>。端口被占用时可在 `.env` 中设置 `PORT=8098` 等其他端口。

#### 6. 运行测试

```bash
go test -count=1 ./cmd/server/
```

---

## 使用流程

1. 打开页面，右上角确认显示“生产引擎就绪”
2. 左侧填写**计划名称**
3. 选择影像来源（二选一）：
   - **上传影像** —— 浏览器选择航飞影像文件夹（保留目录结构）
   - **挂载目录** —— 填写容器内路径，例如 `/mnt/input/site-a/images`
4. 可选：设置预约执行时间；留空则保存为草稿，之后手动启动
5. 点击 **立即重建 OSGB**，或点击 **保存计划** 稍后再启动
6. 右侧查看阶段时间轴与实时日志，完成后点击 **下载 OSGB 成果包**

成果包内容：`root.osgb`、`Data/` 分层瓦片、纹理、`validation_report.json`。
交付路径为 `/data/deliverables/<计划ID>/<任务ID>/`。

### 计划列表的操作

列表中的每个计划都有独立的**开始**按钮：

- **点击计划名称**只是选中它（右侧监控面板会切到该计划），**不会**开始重建
- 点击该行右侧的 **开始** 按钮才会真正启动，避免误触占用整条流水线数小时
- 运行中的计划，“开始”按钮会变为“运行中”并禁用
- 勾选左侧复选框可多选，配合 **全选** 与 **删除选中（N）** 批量删除

删除前会弹出确认框，列出将被删除的计划名称，并提示会连同成果包、任务临时产物和已上传影像一起删除且无法恢复。若某个计划的任务正在运行，它不会被勾选（复选框禁用），因为删除会破坏正在运行的子进程——请先「停止任务」。

### 两种数据入口

| 入口 | 适用情况 | 容器内位置 |
|---|---|---|
| 上传影像 | 操作员从浏览器选择影像文件夹 | 自动保存至 `/data/uploads/...` |
| 挂载目录 | 数据量很大、不希望经过浏览器上传 | 宿主机目录挂载为 `./input:/mnt/input:ro`，页面填 `/mnt/input/子目录` |

---

## 标准开源链路

`PIPELINE_MODE=native`（默认）时按下列阶段运行：

| 阶段 | 组件 | 产物 |
|---|---|---|
| 影像预处理 | Go 标准库 | `images.manifest.json`，记录路径、大小、可读宽高 |
| 特征提取 / 匹配 | COLMAP | `database.db` |
| 空三与束调整 | COLMAP | `sparse/0` |
| 稠密点云 | OpenMVS `InterfaceCOLMAP`、`DensifyPointCloud` | `scene.mvs`、`dense.mvs` |
| 网格重建 / 优化 | OpenMVS `ReconstructMesh`、`RefineMesh` | `mesh.mvs`、`refined.mvs` |
| 纹理映射 | OpenMVS `TextureMesh` | OBJ + 纹理 |
| OSGB 输出 | OpenSceneGraph `osgconv` + 服务内置分层器 | `root.osgb`、`Data/` PagedLOD 瓦片、纹理、metadata、校验报告 |

该链路不调用 ODX。

---

## 配置

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PORT` | `8080` | 服务监听端口 |
| `PIPELINE_MODE` | `native` | `native` 使用 COLMAP/OpenMVS；`external` 使用兼容旧流程的 `RECON_BIN` + `OSGB_BIN` |
| `DATA_DIR` | `./runtime` | 每个任务的中间文件和日志目录；Docker 为 `/data` |
| `CONFIG_FILE` | `runtime/service.env` | Web“工具配置”保存的持久化工具路径文件；Docker 为 `/data/service.env` |
| `COLMAP_BIN` | `colmap` | COLMAP 可执行文件 |
| `OPENMVS_*_BIN` | 对应命令名 | OpenMVS 六个阶段的可执行文件 |
| `OSGCONV_BIN` | `osgconv` | OBJ 到 OSGB 的转换器 |
| `GEOREF_BIN` / `GEOREF_ARGS` | 空 | 可选坐标转换/GCP 校正阶段 |
| `LOD_BIN` / `LOD_ARGS` | 空 | 可选的第三方 LOD 前处理；最终仍由服务生成 Smart3D 分层树 |
| `OUTPUT_ROOT` | 空 | 设置后限制输出路径必须位于该目录内；Docker 为 `/data/deliverables` |
| `COMMAND_TIMEOUT_HOURS` | `24` | 单个外部命令超时 |
| `MAX_UPLOAD_GB` | `50` | 浏览器上传影像包的总大小上限 |
| `COLMAP_MATCHER` | `sequential` | 特征匹配策略：`sequential`、`exhaustive`、`vocab_tree`、`spatial`、`transitive` |
| `COLMAP_MATCHER_OVERLAP` | `10` | `sequential` 策略下每张影像与相邻影像匹配的数量 |
| `PIPELINE_THREADS` | `0` | 各阶段工作线程数；`0` 表示自动（CPU 核数 -1，上限 16） |
| `SIFT_MAX_IMAGE_SIZE` | `3200` | 特征提取的影像长边上限；`0` 表示不限制 |
| `TILE_GRID` | `0` | Smart3D 瓦片网格每轴维度；`0` 表示按模型面数自动选择 |
| `LOD_LEVELS` | `0` | 每瓦片 LOD 层级数；`0` 表示按瓦片面数自动选择 |
| `OSGB_MEMORY_LIMIT` | `12g` | 仅 Compose：容器内存上限 |
| `OSGB_SHM_SIZE` | `1g` | 仅 Compose：共享内存大小 |

`external` 模式仍支持 `{project_dir}`、`{input_dir}`、`{job_dir}`、`{obj_path}`、`{model_path}`、`{output_path}` 占位符。

Web 右上角“工具配置”可编辑 COLMAP、OpenMVS、`osgconv` 及可选工具位置，保存后立即检测可执行性，并写入 `CONFIG_FILE`。容器部署时该文件位于持久化 `/data` 卷；若同名变量同时在 Compose `environment` 中指定，则 Compose 环境变量优先。

---

## 性能与纯 CPU 计算

**本服务全程不使用 GPU。** COLMAP 与 OpenMVS 的所有 GPU 开关都被显式关闭（COLMAP 的 `use_gpu` 默认为 1，必须显式传 0），并且每个子进程都会清空 `CUDA_VISIBLE_DEVICES` 作为第二道保险。因此构建镜像不需要 NVIDIA Container Toolkit，普通 Docker 主机即可运行。

因为全部算力来自 CPU，**线程数与匹配策略是决定总耗时的两个关键参数**。

### 匹配策略（影响最大）

`exhaustive` 会让每张影像与其余所有影像两两匹配，图像对数量随影像数按 O(n²) 增长。以 428 张影像为例：穷举需要比对 **91,378** 对，而航飞影像按航线顺序拍摄，实际上只有相邻帧才可能重叠——其中绝大部分是无用功。

默认的 `sequential` 策略只匹配每张影像的近邻，同样的 428 张影像只需约 **4,280** 对，**减少约 95% 的计算量**。任务日志会明确打印所选策略与候选图像对数量，便于核对：

```text
CPU-only: GPU switches are forced off for every COLMAP and OpenMVS stage; threads=16
matcher=sequential_matcher overlap=10, candidate image pairs=4280
```

若相邻航线之间无法正确连接（表现为空三只恢复出部分影像、或点云出现明显断裂），可增大 `COLMAP_MATCHER_OVERLAP`；只有当影像是无顺序采集（例如随机拍摄的整理照片）时，才应改用 `COLMAP_MATCHER=exhaustive`。

### 线程数

`PIPELINE_THREADS=0`（默认）表示自动：取“可用 CPU 核数 − 1”，上限 16，留一核给服务自身的 HTTP、日志与流式输出。设置成具体数值即可覆盖。上限存在的原因是 COLMAP 与 OpenMVS 会按线程分配缓冲，容器有内存上限，线程数过高会以 OOM 换取速度。

### 何时调低参数

若容器因内存不足被终止（日志出现 `signal: killed`），优先降低 `PIPELINE_THREADS`，其次降低 `SIFT_MAX_IMAGE_SIZE`。

---

## Smart3D 分层树的分块与简化

内置分层器把贴图 OBJ 切成瓦片，每片生成多级 LOD，瓦片命名沿用 Smart3D 风格 `Tile_+003_+012`：

- 瓦片按三角形 **XY 质心**归属，与 Smart3D/S3C 的地图坐标寻址方式一致
- 简化会**锁定瓦片包围盒边界带内的顶点**（带宽为瓦片最大边长的 1/64），相邻瓦片因此在其共享边上采样位置一致，不会在拼接处产生裂缝
- 每一级的顶点数按 4 倍递减（1/4、1/16、1/64…），最细一级始终是未简化的完整网格
- 若地形以陡崖、建筑立面等垂直结构为主，单个瓦片的竖直跨度会远大于其地图投影范围，包围球随之虚大，LOD 切换阈值偏大。此时任务日志会输出 `tile vertical spread ratio` 提示该情况

### 网格与层级自适应

默认（`TILE_GRID=0`、`LOD_LEVELS=0`）会根据模型规模自动选择，避免“小模型切出几百个空瓦片、大模型单瓦片过大”：

| 模型面数 | 网格 | 每瓦片面数 | 层级 |
|---|---|---|---|
| 5 万 | 4×4 | 约 3 千 | 2 |
| 100 万 | 4×4 | 约 6 万 | 3 |
| 3000 万 | 16×16 | 约 12 万 | 3 |

- 网格：按“每瓦片约 12 万面”反推，钳制在 4×4 到 24×24
- 层级：稀疏瓦片只给 1 级（再粗也看不出差别），密集瓦片最多 5 级

要**完全复现旧版固定布局**，设置 `TILE_GRID=16` 与 `LOD_LEVELS=3` 即可；每多一级会在每个瓦片上增加 3 次 `osgconv` 调用，因此层数并非越多越好。任务日志会打印本次实际采用的 `grid=` 与 `lod_levels=`，便于核对。

---

## API

| 接口 | 作用 |
|---|---|
| `GET /health` | 健康检查 |
| `GET /api/config` | 查看管线模式和依赖状态 |
| `PUT /api/config` | 更新工具路径并重新检测可执行性，JSON：`tools` 对象；任务运行或排队时返回 409 |
| `GET /api/pick-directory?kind=input\|output` | Windows 原生路径选择 |
| `GET /api/jobs` | 列出全部任务 |
| `POST /api/jobs` | 创建任务，JSON：`input_path`、`output_path` |
| `GET /api/jobs/:id` | 查询任务状态 |
| `DELETE /api/jobs/:id/cancel` | 停止正在运行的任务 |
| `GET /api/jobs/:id/events` | SSE 实时日志、进度和状态 |
| `GET /api/jobs/:id/download` | 下载完整 OSGB 成果 ZIP。仅限由计划启动且状态为 `completed` 的任务 |
| `POST /api/jobs/:id/resume-dense` | 从已有 `scene.mvs` 继续稠密重建，跳过 COLMAP |
| `POST /api/jobs/:id/resume` | 从最近成功的检查点继续失败的任务（需 `resumable` 为 `true`） |
| `POST /api/uploads` | `multipart/form-data` 上传字段 `files`，可一次上传完整影像目录 |
| `GET /api/plans` | 查询持久化生产计划 |
| `POST /api/plans` | 创建计划，字段：`name`、`upload_id` 或 `input_path`、可选 `scheduled_at`、`start_now` |
| `POST /api/plans/:id/run` | 开始重建该计划 |
| `DELETE /api/plans/:id` | 删除单个计划及其全部产物；该计划不存在时返回 404 |
| `DELETE /api/plans` | 批量删除，JSON：`{"ids": ["...", "..."]}`；幂等，已不存在的 id 不算失败 |

### 删除计划

删除计划会同时清理它的**全部产物与临时产物**：

| 内容 | 路径 |
|---|---|
| 计划记录 | `plans/<计划ID>.json` |
| 任务工作区（含 `database.db`、`*.mvs`、日志、检查点） | `jobs/<任务ID>/` |
| 交付成果（`root.osgb`、`Data/`、纹理、校验报告） | `deliverables/<计划ID>/` |
| 已上传的影像 | `uploads/<上传ID>/` |

两条安全约束：

- **共用影像不会被误删。** 若同一批上传影像被另一个仍然存在的计划引用，该上传目录会被保留（响应中的 `upload_retained` 会说明），只有删除最后一个引用它的计划时才真正删除。
- **运行中的计划不允许删除。** 子进程仍持有 `jobs/<任务ID>/` 下的文件，此时删除会破坏运行。请先「停止任务」再删除；接口会返回 409 并说明原因。

批量删除是**幂等**的：删除一个已经不存在（或已被他人删除）的计划不算失败，而是成功——结果里会把它列在 `missing` 中。只有"计划存在但此刻不能删"才算失败。

| 状态码 | 含义 |
|---|---|
| `200` | 至少删除了一个；或请求的 id 全部已不存在 |
| `207` | 部分删除成功、部分被拒绝；body 同时给出 `deleted` 与 `failed` |
| `409` | 一个都没删掉，因为计划正在运行 |
| `404` | 仅当用 `DELETE /api/plans/:id` 点名某一个计划且它不存在时 |

响应示例：

```json
{
  "deleted": [
    { "plan_id": "...", "name": "矿区 2026-09-21", "jobs_deleted": ["..."],
      "upload_deleted": "upload-...", "deliverables_deleted": true,
      "freed_bytes": 1837465920 }
  ],
  "missing": ["已不存在的计划ID"],
  "failed": { "某个计划ID": "plan \"...\" still has a running job (...)" },
  "total": 2,
  "succeeded": 1
}
```

### 瓦片校验与部分交付

每个任务完成时都会用 `osgconv` 回读校验全部 OSGB 瓦片，并写入 `validation_report.json`：

| 失败瓦片比例 | 结果 |
|---|---|
| 0 | 任务成功 |
| 大于 0 且不超过 10% | 任务成功，按部分交付处理；失败瓦片明细写入日志与 `validation_report.json`，`job.stats.failed_tiles` 反映真实数量 |
| 超过 10%，或全部瓦片失败 | 任务失败，错误信息包含失败数量、比例和前 10 个失败原因 |

该策略保证长时间任务不会因为个别瓦片异常而丢弃全部成果，同时避免把损坏过多的成果当作可用交付物。

### 任务恢复

管线中的每个阶段完成后都会写入 `.checkpoint-<阶段名>` 文件。服务重启或任务失败后：

- 状态为 `failed` 且 `resumable` 为 `true` 的任务可通过 `POST /api/jobs/:id/resume` 继续，已完成的阶段会被跳过
- 可使用 `POST /api/jobs/:id/resume-dense` 直接从已有 `scene.mvs` 恢复，完全跳过 COLMAP 阶段
- 服务在运行中停止时，重启后会把 `running`/`queued` 任务标记为 `failed` 并置为可恢复，不会停留在“永远运行中”状态

---

## COLMAP 版本兼容

COLMAP 在 4.0 版本重命名了 SIFT 相关选项组：旧版（≤ 3.9）为 `--SiftExtraction.*` / `--SiftMatching.*`，新版（≥ 4.0）为 `--FeatureExtraction.*` / `--FeatureMatching.*`。**两组拼写互斥，传错会直接报 `unrecognised option` 并退出。**

服务会在运行时探测实际使用的 COLMAP 二进制，并自动选择正确的选项组，因此本机与容器版本不同时无需手工配置。探测结果会缓存，不会对每个任务重复执行。Docker 镜像中的 COLMAP 为 Ubuntu 24.04 提供的 **3.9.1（without CUDA）**。

镜像构建时会把容器内 COLMAP 的版本写入 `/opt/colmap-version.txt`，便于排查：

```bash
docker compose exec osgb-service cat /opt/colmap-version.txt
```

---

## OpenMVS PLY 与 osgconv 兼容性

OpenMVS 生成的二进制 PLY 可能使用 `property list uint8 uint32 vertex_indices`。OSG 3.6.5 的 PLY 插件虽能识别 `float32/uint8` 等属性类型，但不能识别 `uint32`，转换时会报 `get_binary_item: bad type = 0` 并产生空/残缺 Geode。

服务会在转换前自动修复 PLY 头：

| 处理项 | 原类型 | 替换类型 | 说明 |
|---|---|---|---|
| 面索引数量类型 | `uint8` | `uchar` | 二进制保持不变 |
| 面索引数据类型 | `uint32` | `int` | 二进制保持不变，只改声明 |

转换完成后，服务会用 `osgconv` 将 OSGB 回读为文本并解析：

| 校验项 | 失败条件 |
|---|---|
| 顶点 | OSGB 中无顶点，或顶点数明显少于 PLY 声明值 |
| 三角面 | OSGB 中无索引/面片，或面片数明显少于 PLY 声明值 |
| 回读 | `osgconv` 无法读取输出 OSGB |

校验结果会写入任务日志、`job.json` 的 `stats` 字段和 UI 的任务信息区。

---

## 坐标与长期交付说明

OpenMVS 默认输出局部坐标。要交付绝对地理坐标，照片 EXIF 或 GCP 必须包含可靠的坐标和高程基准，并在生产环境增加坐标转换/检查步骤。服务会保留每个任务的清单、中间产物、日志与计划；任务和计划索引均持久化在 `/data`，服务重启后会恢复到 Web 列表。

默认 `OPENMVS_DENSIFY_ARGS` 已使用 CPU 稠密重建/融合；已有 `depth*.dmap` 时，OpenMVS 会复用它们并直接进入融合。若自行覆盖该变量，请不要加入 `--cuda-device`。

Docker 镜像安装的是 Ubuntu 软件包版 COLMAP、OpenMVS 与 OpenSceneGraph，适合 CPU 验证与小规模生产。Windows 的 `.exe` 不能直接复制进 Linux 容器使用。

---

## Windows 路径写法

工具路径与命令参数模板支持正斜杠和反斜杠两种写法，例如下面两种都能正确解析：

```ini
OSGCONV_BIN=E:/OpenSceneGraph-3.6.5/bin/osgconv.exe
OSGCONV_BIN=E:\OpenSceneGraph-3.6.5\bin\osgconv.exe
```

含空格的路径请用双引号包裹。注意：形如 `"C:\dir\"` 的写法中，结尾反斜杠紧邻右引号，与“转义引号”存在本质歧义，解析器会报 `unterminated quote`；请去掉结尾反斜杠，或改用正斜杠 `"C:/dir/"`。

---

## 开发

```bash
# 后端：热重载式开发
go run ./cmd/server

# 前端：独立开发服务器（自动代理 /api 到 8080）
cd frontend && npm run dev
```

提交前请确保以下检查全部通过：

```bash
gofmt -l ./cmd/                 # 应无输出
go vet ./...
golangci-lint run ./...         # 配置见 .golangci.yml，应零告警
go test -count=1 ./cmd/server/
```

代码结构：

| 文件 | 职责 |
|---|---|
| `cmd/server/main.go` | 配置、任务与计划管理、流水线编排、PLY 兼容、HTTP API |
| `cmd/server/colmap_args.go` | COLMAP 选项组探测与 CPU-only 参数构建 |
| `cmd/server/pagedlod.go` | OBJ 解析、瓦片切分、网格简化、OBJ 导出 |
| `cmd/server/pagedlod_tree.go` | PagedLOD 树构建、纹理降采样、OSGB 校验与报告 |
| `frontend/src/App.vue` | 整页 UI |

`.golangci.yml` 中每一项豁免都写明了理由（例如作业刻意不继承 HTTP 请求的 context，因为重建任务必须在浏览器断开后继续运行）。

---

## 疑难排查

| 现象 | 原因与处理 |
|---|---|
| 页面显示“等待重建引擎配置” | 7 个工具未全部就绪。打开右上角“工具配置”查看哪一个未检测到 |
| 容器 unhealthy | 镜像构建缺失工具。执行 `docker compose logs osgb-service` 查看详情 |
| 日志出现 `signal: killed` | 容器内存不足。降低 `PIPELINE_THREADS`，或提高 `OSGB_MEMORY_LIMIT` |
| 空三只恢复部分影像 / 点云断裂 | 相邻航线未连接。增大 `COLMAP_MATCHER_OVERLAP` |
| `unrecognised option '--Sift...'` | COLMAP 版本与选项组不匹配。本服务会自动探测，若出现请检查 `COLMAP_BIN` 是否指向了预期版本 |
| 瓦片无纹理 | `osgconv` 缺少 `osgPlugins-*` 目录或 DLL。不要只复制单个 `osgconv.exe` |
| 端口 8080 被占用 | 在 `.env` 中设置 `PORT=8098`，并同步修改 `compose.yaml` 的端口映射 |
| 下载按钮不可用 | 成果包仅支持**由计划启动**且状态为 `completed` 的任务 |
| 上次任务卡在“运行中” | 服务重启后会自动标记为 `failed` 且可恢复，点击“从检查点继续”即可 |
