#!/bin/bash

set -eu

LOCAL_PATH="."

for i in $(seq 1 3); do
  host="isu${i}"
  echo $host
  rsync -avr "${LOCAL_PATH}/webapp/go/" ${host}:~/webapp/go/
  rsync -avr "${LOCAL_PATH}/webapp/sql/" ${host}:~/webapp/sql/
  rsync -avr "${LOCAL_PATH}/env.sh.${i}" ${host}:~/env.sh
  ssh ${host} 'bash -l -c "sudo logrotate -f /etc/logrotate.conf; export PATH=/home/isucon/local/go/bin:/home/isucon/go/bin:\$PATH; cd ~/webapp/go; rm -f isureal; go build -o isureal ./cmd/isureal; sudo systemctl restart isureal-go.service;"'
done
