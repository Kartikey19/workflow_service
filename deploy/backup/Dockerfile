FROM postgres:16-alpine
RUN apk add --no-cache python3 py3-boto3
COPY backup.py restore.py test_backup.py /usr/local/bin/
COPY crontab /etc/crontabs/root
RUN mkdir -p /backups && chmod 700 /backups
ENV TZ=UTC
CMD ["crond", "-f", "-l", "8"]
