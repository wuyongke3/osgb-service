# 航飞影像 OSGB 生产服务

本项目是一个可容器化部署的单用户生产服务：Vue 3 + Vite + TypeScript 提供影像上传、生产计划、阶段组态和 SSE 实时日志；Go 服务负责任务目录、进程编排、Smart3D 风格分层 OSGB、metadata 写入、回读校验和成果下载。上传的照片只保存在部署者挂载的 `/data` 卷中。

## 标准开源链路（默认）

`PIPELINE_MODE=native` 时，服务按下列阶段运行：

| 阶段 | 组件 | 产物 |
|---|---|---|
| 影像预处理 | Go 标准库 | `images.manifest.json`，记录路径、大小、可读宽高 |
| 特征提取 / 匹配 | COLMAP | `database.db` |
| 空三与束调整 | COLMAP | `sparse/0` |
| 稠密点云 | OpenMVS `InterfaceCOLMAP`、`DensifyPointCloud` | `scene.mvs`、`dense.mvs` |
| 网格重建 / 优化 | OpenMVS `ReconstructMesh`、`RefineMesh` | `mesh.mvs`、`refined.mvs` |
| 纹理映射 | OpenMVS `TextureMesh` | OBJ + 纹理 |
| OSGB 输出 | OpenSceneGraph `osgconv` + 服务内置分层器 | `root.osgb`、`Data/` PagedLOD 瓦片、纹理、metadata、校验报告 |

该链路不调用 ODX。默认配置为纯 CPU：COLMAP 的特征提取与匹配关闭 GPU，OpenMVS 稠密重建不传递 CUDA 设备参数，且所有子进程都会清空 `CUDA_VISIBLE_DEVICES`，因此 Docker/Linux 部署不需要 NVIDIA Container Toolkit。CPU 重建速度会明显慢于经过验证的 CUDA 环境，可通过 `COLMAP_MATCHER` 与 `PIPELINE_THREADS` 调优，详见下文“性能与纯 CPU 计算”。

## 安装依赖

需要将以下程序放入 `PATH`，或在 `.env` 中填写绝对路径：`colmap`、`InterfaceCOLMAP`、`DensifyPointCloud`、`ReconstructMesh`、`RefineMesh`、`TextureMesh`、`osgconv`。

### Windows OpenSceneGraph 下载

OpenSceneGraph 官方稳定版页面列出的 Windows 64 位构建来自 Objexx Engineering：

<https://objexx.com/OpenSceneGraph.html>

当前可用包为 `OpenSceneGraph 3.6.5 - VC2022 - 64-bit`。下载 `.7z` 后用 7-Zip 解压，例如 `E:\OpenSceneGraph-3.6.5`，确认其中存在 `bin\osgconv.exe`。然后在 `.env` 中填写：

```text
OSGCONV_BIN=E:/OpenSceneGraph-3.6.5/bin/osgconv.exe
```

也可以把该 `bin` 目录加入系统 `PATH`，再保持 `OSGCONV_BIN=osgconv`。运行时需保留压缩包解压出的 DLL 和 `osgPlugins-*` 目录，不要只复制单个 `osgconv.exe`。

复制 `.env.example` 为 `.env` 后，可通过 `GET /api/config` 查看每个依赖是否可执行。标准链路会将贴图 OBJ 转为 Smart3D 风格的 PagedLOD OSGB 树：`root.osgb`、`Data/` 分层瓦片与纹理；每个瓦片和根节点均写入 OSG `UserDataContainer` metadata，并在完成时用 `osgconv` 回读检查层级、贴图、几何和 metadata。

## 启动

```powershell
Set-Location osgb-service
Copy-Item .env.example .env
Set-Location frontend
npm install
npm run build
Set-Location ..
go run ./cmd/server
```

浏览器打开 `http://localhost:8080`。端口被占用时设置 `PORT=8098` 等其他端口。

## 配置

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PIPELINE_MODE` | `native` | `native` 使用 COLMAP/OpenMVS；`external` 使用兼容旧流程的 `RECON_BIN` + `OSGB_BIN` |
| `DATA_DIR` | `./runtime` | 每个任务的中间文件和日志目录 |
| `CONFIG_FILE` | `runtime/service.env` | Web“工具配置”保存的持久化工具路径文件；Docker 为 `/data/service.env` |
| `COLMAP_BIN` | `colmap` | COLMAP 可执行文件 |
| `OPENMVS_*_BIN` | 对应命令名 | OpenMVS 六个阶段的可执行文件 |
| `OSGCONV_BIN` | `osgconv` | OBJ 到 OSGB 的转换器 |
| `GEOREF_BIN` / `GEOREF_ARGS` | 空 | 可选坐标转换/GCP 校正阶段 |
| `LOD_BIN` / `LOD_ARGS` | 空 | 可选的第三方 LOD 前处理；最终仍由服务生成 Smart3D 分层树 |
| `OUTPUT_ROOT` | 空 | 设置后限制输出路径必须位于该目录内 |
| `COMMAND_TIMEOUT_HOURS` | `24` | 单个外部命令超时 |
| `MAX_UPLOAD_GB` | `50` | 浏览器上传影像包的总大小上限 |
| `COLMAP_MATCHER` | `sequential` | 特征匹配策略：`sequential`、`exhaustive`、`vocab_tree`、`spatial`、`transitive` |
| `COLMAP_MATCHER_OVERLAP` | `10` | `sequential` 策略下每张影像与相邻影像匹配的数量 |
| `PIPELINE_THREADS` | `0` | 各阶段工作线程数；`0` 表示自动（CPU 核数 -1，上限 16） |
| `SIFT_MAX_IMAGE_SIZE` | `3200` | 特征提取的影像长边上限；`0` 表示不限制 |

`external` 模式仍支持 `{project_dir}`、`{input_dir}`、`{job_dir}`、`{obj_path}`、`{model_path}`、`{output_path}` 占位符。

Web 右上角“工具配置”可编辑 COLMAP、OpenMVS、`osgconv` 及可选工具位置，保存后立即检测可执行性，并写入 `CONFIG_FILE`。容器部署时，该文件位于持久化 `/data` 卷；若同名变量同时在 Docker Compose `environment` 中指定，则 Compose 环境变量优先。

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

`PIPELINE_THREADS=0`（默认）表示自动：取“可用 CPU 核数 − 1”，上限 16，留一核给服务自身的 HTTP、日志与流式输出。设置成具体数值即可覆盖。上限存在的原因是 COLMAP 与 OpenMVS 会按线程分配缓冲，容器有内存上限（Compose 默认 12 GB），线程数过高会以 OOM 换取速度。

### 何时调低参数

若容器因内存不足被终止（日志出现 `signal: killed`），优先降低 `PIPELINE_THREADS`，其次降低 `SIFT_MAX_IMAGE_SIZE`。

## COLMAP 版本兼容

COLMAP 在 4.0 版本重命名了 SIFT 相关选项组：旧版（≤ 3.9）为 `--SiftExtraction.*` / `--SiftMatching.*`，新版（≥ 4.0）为 `--FeatureExtraction.*` / `--FeatureMatching.*`。**两组拼写互斥，传错会直接报 `unrecognised option` 并退出。**

服务会在运行时探测实际使用的 COLMAP 二进制，并自动选择正确的选项组，因此本机与容器版本不同时无需手工配置。探测结果会缓存，不会对每个任务重复执行。若探测失败（二进制缺失等），会回退到旧版拼写，与 Docker 镜像中的 COLMAP 3.9.1 匹配。

Docker 镜像构建时会把容器内 COLMAP 的版本写入 `/opt/colmap-version.txt`，便于排查：

```bash
docker compose exec osgb-service cat /opt/colmap-version.txt
```

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
| `GET /api/jobs/:id/download` | 下载完整 OSGB 成果 ZIP（root、Data、纹理、校验报告）。仅限由计划启动且状态为 `completed` 的任务 |
| `POST /api/jobs/:id/resume-dense` | 从已有 `scene.mvs` 继续稠密重建，跳过 COLMAP |
| `POST /api/jobs/:id/resume` | 从最近成功的检查点继续失败的任务（需 `resumable` 为 `true`） |
| `POST /api/uploads` | `multipart/form-data` 上传字段 `files`，可一次上传完整影像目录 |
| `GET /api/plans` | 查询持久化生产计划 |
| `POST /api/plans` | 创建计划，字段：`name`、`upload_id` 或 `input_path`、可选 `scheduled_at`、`start_now` |
| `POST /api/plans/:id/run` | 手动运行草稿或已完成计划 |

### 瓦片校验与部分交付

每个任务完成时都会用 `osgconv` 回读校验全部 OSGB 瓦片，并写入 `validation_report.json`。校验策略为：

| 失败瓦片比例 | 结果 |
|---|---|
| 0 | 任务成功 |
| 大于 0 且不超过 10% | 任务成功，按部分交付处理；失败瓦片明细写入日志与 `validation_report.json`，`job.stats.failed_tiles` 反映真实数量 |
| 超过 10%，或全部瓦片失败 | 任务失败，错误信息包含失败数量、比例和前 10 个失败原因 |

该策略保证长时间任务不会因为个别瓦片异常而丢弃全部成果，同时避免把损坏过多的成果当作可用交付物。

### 任务恢复

管线中的每个阶段完成后都会写入 `.checkpoint-<阶段名>` 文件。服务重启或任务失败后：

- 状态为 `failed` 且 `resumable` 为 `true` 的任务可通过 `POST /api/jobs/:id/resume` 继续，已完成的阶段会被跳过；
- 可使用 `POST /api/jobs/:id/resume-dense` 直接从已有 `scene.mvs` 恢复，完全跳过 COLMAP 阶段；
- 服务在运行中停止时，重启后会把 `running`/`queued` 任务标记为 `failed` 并置为可恢复，不会停留在"永远运行中"状态。


## OpenMVS PLY 与 osgconv 兼容性

OpenMVS 生成的二进制 PLY 可能使用 `property list uint8 uint32 vertex_indices`。OSG 3.6.5 PLY 插件虽能识别 `float32/uint8` 等属性类型，但不能识别 `uint32`，转换时会报 `get_binary_item: bad type = 0` 并产生空/残缺 Geode。

服务已在 `runNative` 和 `runDenseToOSGB` 中自动修复该 PLY，并在 OSGB 写出后强制回读校验：

| 处理项 | 原类型 | 替换类型 | 说明 |
|---|---|---|---|
| 面索引数量类型 | `uint8` | `uchar` | 二进制保持不变 |
| 面索引数据类型 | `uint32` | `int` | 二进制保持不变，只改声明 |

转换完成后，服务会用 `osgconv` 将 OSGB 回读为 OSGB 文本，并解析：

| 校验项 | 失败条件 |
|---|---|
| 顶点 | OSGB 中无顶点，或顶点数明显少于 PLY 声明值 |
| 三角面 | OSGB 中无索引/面片，或面片数明显少于 PLY 声明值 |
| 回读 | `osgconv` 无法读取输出 OSGB |

校验结果会写入任务日志、`job.json` 的 `stats` 字段和 UI 的任务信息区。

## Smart3D 分层树的分块与简化

内置分层器把贴图 OBJ 切成 16 × 16 瓦片，每片生成 3 级 LOD（1/16 面、1/4 面、完整网格），瓦片命名沿用 Smart3D 风格 `Tile_+003_+012`：

- 瓦片按三角形 **XY 质心**归属，与 Smart3D/S3C 的地图坐标寻址方式一致。
- 简化会**锁定瓦片包围盒边界带内的顶点**（带宽为瓦片最大边长的 1/64），相邻瓦片因此在其共享边上采样位置一致，不会在拼接处产生裂缝。
- 若地形以陡崖、建筑立面等垂直结构为主，单个瓦片的竖直跨度会远大于其地图投影范围，包围球随之虚大，LOD 切换阈值偏大。此时任务日志会输出 `tile vertical spread ratio` 提示该情况，便于判断扁平 XY 网格是否适合本次数据。

## 坐标与长期交付说明

OpenMVS 默认输出局部坐标。要交付绝对地理坐标，照片 EXIF 或 GCP 必须包含可靠的坐标和高程基准，并在生产环境增加坐标转换/检查步骤。服务会保留每个任务的清单、中间产物、日志与计划；任务和计划索引均持久化在 `/data`，服务重启后会恢复到 Web 列表。

默认 `OPENMVS_DENSIFY_ARGS` 已使用 CPU 稠密重建/融合；已有 `depth*.dmap` 时，OpenMVS 会复用它们并直接进入融合。若自行覆盖该变量，请不要加入 `--cuda-device`。

## Windows 路径写法

`.env` 中的工具路径与命令参数模板支持正斜杠和反斜杠两种写法，例如下面两种都能正确解析：

```text
OSGCONV_BIN=E:/OpenSceneGraph-3.6.5/bin/osgconv.exe
OSGCONV_BIN=E:\OpenSceneGraph-3.6.5\bin\osgconv.exe
```

含空格的路径请用双引号包裹。注意：形如 `"C:\dir\"` 的写法中，结尾反斜杠紧邻右引号，与"转义引号"存在本质歧义，解析器会报 `unterminated quote`；请去掉结尾反斜杠，或改用正斜杠 `"C:/dir/"`。

## Docker 部署

项目根目录已经提供 `Dockerfile`、`compose.yaml` 和 `.dockerignore`。镜像在一个容器中运行服务端与 Web 端：`/` 是 Web UI，`/api` 是 Go API。数据卷 `/data` 会持久化上传影像、计划、任务日志和交付成果，重启容器不会丢失计划或已完成任务。

```bash
cd osgb-service
mkdir -p runtime input
docker compose up -d --build
```

也可以执行项目内的一键安装脚本：

```bash
bash ./scripts/install-tools.sh
```

脚本会校验 Docker Compose、构建镜像并由 `Dockerfile` 自动安装 COLMAP、OpenMVS
和 OpenSceneGraph，最后重建服务容器。Web 服务本身以非 root 用户运行，不会在请求中执行
`apt install`，也不会挂载 Docker socket；这避免了让浏览器端获得宿主机 root 权限，同时保证
工具版本随镜像可复现。右上角“工具配置”弹窗会显示该 Docker/Linux 安装命令并提供复制按钮。

容器健康检查会同时检测 COLMAP、OpenMVS 和 `osgconv` 是否全部可执行；若镜像构建缺失工具，
容器会显示为 `unhealthy`，Web 的环境检测也会显示未检测到的具体工具。

浏览器访问 `http://localhost:8080`。Web 页面提供两种数据入口：

| 入口 | 适用情况 | 容器内位置 |
|---|---|---|
| 上传影像 | 操作员从浏览器选择一个航飞影像文件夹 | 自动保存至 `/data/uploads/...` |
| 挂载目录 | 数据量很大、不希望浏览器上传 | 将宿主机目录挂载为 `./input:/mnt/input:ro`，在页面填 `/mnt/input/子目录` |

每次由计划启动的任务都会写入 `/data/deliverables/<plan-id>/<job-id>/`，完成后页面“下载 OSGB 成果包”会下载该目录的 ZIP，包含 `root.osgb`、`Data/` 层级瓦片、纹理和 `validation_report.json`。

默认 Dockerfile 安装 Ubuntu 软件包版 COLMAP、OpenMVS 与 OpenSceneGraph，适合 CPU 验证与小规模生产。镜像中的 COLMAP 为 Ubuntu 24.04 提供的 **3.9.1（without CUDA）**；服务会自动适配其选项组拼写，无需手工配置。GPU 生产应从此镜像派生，替换为已在目标显卡验证过的 CUDA 版 COLMAP/OpenMVS，并配置 NVIDIA Container Toolkit；服务接口与数据卷无需改变（服务本身只按 CPU 方式调用这些工具）。Windows 的 `.exe` 不能直接复制进 Linux 容器使用。
