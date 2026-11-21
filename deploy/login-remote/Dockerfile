# 远程浏览器 sidecar：仅用于「服务器 IP 上完成 OAuth 登录」这一个场景。
#
# 为什么不并进主镜像：主镜像 alpine + 80MB，塞 Chromium 要 +350MB，且 musl 与
# playwright 官方包不兼容（必须 glibc）。独立容器把体积与故障域隔开，
# 主服务只在 env 里多一个地址。
FROM python:3.13-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    # 固定浏览器路径：镜像内以非 root 运行时要读得到
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

WORKDIR /app
COPY requirements.txt .
RUN pip install -r requirements.txt \
 && playwright install --with-deps chromium \
 # CJK 字体：缺了登录页中文全是方块，等于看不见验证提示
 && apt-get update \
 && apt-get install -y --no-install-recommends fonts-wqy-microhei \
 && rm -rf /var/lib/apt/lists/* \
 && chmod -R 755 /ms-playwright

COPY server.py .

RUN useradd -u 10001 -m app && chown -R app:app /app
USER 10001
EXPOSE 8099

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD python -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:8099/healthz',timeout=4).read()" || exit 1

CMD ["python", "server.py"]
