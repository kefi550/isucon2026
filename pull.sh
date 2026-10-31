#!/bin/bash

set -eu

LOCAL_PATH="."

mkdir -p ${LOCAL_PATH}/webapp/go/
rsync -av isu1:~/webapp/go/ "${LOCAL_PATH}/webapp/go/"

mkdir -p ${LOCAL_PATH}/webapp/sql/
rsync -avr isu1:~/webapp/sql/ "${LOCAL_PATH}/webapp/sql/"

for i in $(seq 1 3); do
  host="isu${i}"

  rsync -avr ${host}:~/env.sh "${LOCAL_PATH}/env.sh.${i}"

  mkdir -p ${LOCAL_PATH}/${host}/etc/systemd/system/
  rsync -avr ${host}:/etc/hosts "${LOCAL_PATH}/${host}/etc/"
  rsync -avr ${host}:/etc/systemd/system/isureal-go.service "${LOCAL_PATH}/${host}/etc/systemd/system/"
  rsync -avr ${host}:/etc/systemd/system/isureal-guard.service "${LOCAL_PATH}/${host}/etc/systemd/system/"

  mkdir -p ${LOCAL_PATH}/${host}/etc/nginx/
  rsync -avr ${host}:/etc/nginx/nginx.conf "${LOCAL_PATH}/${host}/etc/nginx/nginx.conf"
  rsync -avr ${host}:/etc/nginx/sites-enabled/ "${LOCAL_PATH}/${host}/etc/nginx/sites-enabled/"

  mkdir -p ${LOCAL_PATH}/${host}/etc/logrotate.d/
  rsync -avr ${host}:/etc/logrotate.d/ "${LOCAL_PATH}/${host}/etc/logrotate.d/"
done


host=database
mkdir -p ${LOCAL_PATH}/${host}/etc/postgresql/16/main/
rsync -avr ${host}:/etc/postgresql/16/main/postgresql.conf "${LOCAL_PATH}/${host}/etc/postgresql/16/main/postgresql.conf"
rsync -avr ${host}:/etc/postgresql/16/main/conf.d/ "${LOCAL_PATH}/${host}/etc/postgresql/16/main/conf.d/"


