FROM node:22-bookworm-slim AS web-build
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.26-bookworm AS server-build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/osgb-service ./cmd/server

# OpenMVS is not available as an Ubuntu 24.04 apt package.
# Build a pinned OpenMVS release from the official source repository.
FROM ubuntu:24.04 AS openmvs-build

ARG OPENMVS_REF=v2.4.0
# OpenMVS v2.4.0 requires isotropic_remeshing.h, which is not included in
# the old v1.0.1 snapshot. Pin a VCG commit that contains that header.
ARG VCG_REF=658ba36d0a5666650da6e066b4794efc5a463407

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates \
       git \
       build-essential \
       cmake \
       pkg-config \
       libboost-all-dev \
       libeigen3-dev \
       libcgal-dev \
       libopencv-dev \
       libceres-dev \
       libgflags-dev \
       libgoogle-glog-dev \
       libnanoflann-dev \
       libflann-dev \
       libsqlite3-dev \
       libjpeg-dev \
       libpng-dev \
       libtiff-dev \
       libxxf86vm-dev \
       libxi-dev \
       libxrandr-dev \
       libxinerama-dev \
       libglfw3-dev \
       libglew-dev \
       libcurl4-openssl-dev \
       libgdal-dev \
    && rm -rf /var/lib/apt/lists/*

# JPEG XL is an optional Ubuntu package, but OpenMVS enables it when present.
# Keep it in a separate layer so changes to this small dependency do not
# invalidate the large base toolchain layer above.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libjxl-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

RUN git clone \
      --branch "${OPENMVS_REF}" \
      --depth 1 \
      --recurse-submodules \
      https://github.com/cdcseacave/openMVS.git

RUN git init vcglib \
    && git -C vcglib remote add origin https://github.com/cdcseacave/VCG.git \
    && git -C vcglib fetch --depth 1 origin "${VCG_REF}" \
    && git -C vcglib checkout --detach FETCH_HEAD

WORKDIR /src/openMVS

# OpenMVS v2.4.0 references OpenCV's JPEG-XL writer option in SaveImage(),
# but Ubuntu 24.04 ships OpenCV 4.6, which does not expose that enum. The
# native OpenMVS ImageJXL implementation remains enabled through libjxl;
# use the compatible JPEG quality enum for this generic fallback path.
RUN sed -i 's/cv::IMWRITE_JPEGXL_QUALITY/cv::IMWRITE_JPEG_QUALITY/' libs/Common/Types.inl
# Ubuntu 24.04's CGAL package keeps the 3D AABB traits under the generic
# compatibility header. OpenMVS v2.4.0 includes the removed legacy filename.
RUN sed -i \
      -e 's#<CGAL/AABB_traits_3.h>#<CGAL/AABB_traits.h>#' \
      -e 's#<CGAL/AABB_triangle_primitive_3.h>#<CGAL/AABB_triangle_primitive.h>#' \
      libs/MVS/SceneReconstruct.cpp

RUN cmake -S . -B build \
      -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_INSTALL_PREFIX=/opt/openmvs \
      -DINSTALL_BIN_DIR=bin \
      -DINSTALL_LIB_DIR=lib \
      -DVCG_ROOT=/src/vcglib \
      -DOpenMVS_BUILD_TOOLS=ON \
      -DOpenMVS_BUILD_VIEWER=OFF \
      -DOpenMVS_USE_PYTHON=OFF \
      -DOpenMVS_USE_CUDA=OFF \
      -DOpenMVS_USE_CERES=OFF \
      -DOpenMVS_USE_OPENMP=ON \
      -DOpenMVS_ENABLE_TESTS=OFF \
      -DOpenMVS_ENABLE_IPO=OFF \
    && cmake --build build --parallel 2 \
    && cmake --install build \
    && mkdir -p /opt/openmvs/bin \
    && cp -a /opt/openmvs/src/openMVS/bin/OpenMVS/. /opt/openmvs/bin/ \
    && test -x /opt/openmvs/bin/InterfaceCOLMAP \
    && test -x /opt/openmvs/bin/DensifyPointCloud \
    && test -x /opt/openmvs/bin/ReconstructMesh \
    && test -x /opt/openmvs/bin/RefineMesh \
    && test -x /opt/openmvs/bin/TextureMesh

FROM ubuntu:24.04 AS runtime

ENV DEBIAN_FRONTEND=noninteractive \
    PORT=8080 \
    DATA_DIR=/data \
    OUTPUT_ROOT=/data/deliverables \
    PIPELINE_MODE=native \
    QT_QPA_PLATFORM=offscreen \
    OSGCONV_BIN=osgconv \
    COLMAP_MATCHER=sequential \
    COLMAP_MATCHER_OVERLAP=10 \
    PIPELINE_THREADS=0 \
    SIFT_MAX_IMAGE_SIZE=3200 \
    OPENMVS_DENSIFY_ARGS="{scene_mvs} -o {dense_mvs} --resolution-level 2 --max-resolution 1600 --number-views 4 --sub-resolution-levels 0 --fusion-mode 0" \
    PATH=/opt/openmvs/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    LD_LIBRARY_PATH=/opt/openmvs/lib:/usr/local/lib

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates \
       tzdata \
       openscenegraph \
       colmap \
       libboost-all-dev \
       libeigen3-dev \
       libcgal-dev \
       libopencv-dev \
       libceres-dev \
       libgflags-dev \
       libgoogle-glog-dev \
       libflann-dev \
       libsqlite3-0 \
       libjpeg-turbo8 \
       libpng16-16 \
       libtiff6 \
       libxxf86vm1 \
       libxi6 \
       libxrandr2 \
       libxinerama1 \
       libglfw3 \
       libglew2.2 \
       libcurl4 \
       libgdal34t64 \
    && rm -rf /var/lib/apt/lists/*

# Record the bundled COLMAP version. The SIFT option group names changed between
# COLMAP 3.x (--SiftExtraction.*) and 4.x (--FeatureExtraction.*), and the two
# spellings are mutually exclusive. The service probes the binary at run time and
# picks the right one, so this file exists to make the deployed version visible
# when diagnosing a job log. The image intentionally ships the CPU-only build.
RUN colmap --help | head -1 | tee /opt/colmap-version.txt \
    && ( colmap --help | head -1 | grep -qi 'without CUDA' \
         || echo "WARNING: this COLMAP was built with CUDA; the pipeline still forces CPU-only operation" )

RUN apt-get update \
    && apt-get install -y --no-install-recommends libjxl0.7 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=openmvs-build /opt/openmvs/ /opt/openmvs/
COPY --from=server-build /out/osgb-service /app/osgb-service
COPY --from=web-build /src/web /app/web

RUN echo "/opt/openmvs/lib" > /etc/ld.so.conf.d/openmvs.conf \
    && ldconfig \
    && useradd --system --uid 10001 --create-home appuser \
    && mkdir -p /data \
    && chown -R appuser:appuser /app /data

RUN command -v colmap \
    && command -v osgconv \
    && command -v InterfaceCOLMAP \
    && command -v DensifyPointCloud \
    && command -v ReconstructMesh \
    && command -v RefineMesh \
    && command -v TextureMesh

WORKDIR /app
USER appuser
VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
    CMD ["/app/osgb-service", "-healthcheck"]

ENTRYPOINT ["/app/osgb-service"]
