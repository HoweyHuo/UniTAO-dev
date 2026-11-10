# mongodb

a MongoDB instance for UniTAO's Data Services, as its own compose project. it creates the Docker network `mongo_net`; the Data Services of [`shared-mirror`](../shared-mirror) and [`shared-mirror-git`](../shared-mirror-git) join it as an external network and reach MongoDB as `mongodb://mongodb:27017`. one instance can serve several Data Services, each with its own database.

skip this if you already run a MongoDB: put its container on a network and point the Data Services at it (`UNITAO_MONGODB_NETWORK`, endpoint, user and password in their configs).

## Setup

```
cd docker-compose/mongodb
printf 'UNITAO_UID=%s\nUNITAO_GID=%s\n' "$(id -u)" "$(id -g)" > .env   # MongoDB runs as you, its files in data/ are yours
echo 'MONGODB_ROOT_PASSWORD=<choose a password>' >> .env
mkdir -p data                     # bind-mount source: create it yourself, docker would create it owned by root
docker compose up -d --wait
```

 - the root account (`root` / `MONGODB_ROOT_PASSWORD`) is created only on the first start, while `data/` is empty. changing the password in `.env` later changes nothing; change it in MongoDB (`db.changeUserPassword`).
 - the databases live in `data/` and survive `docker compose down` and recreating the container.
 - port 27017 is published on `127.0.0.1` only, for tools on the host (`mongosh`, Compass); UniTAO goes through the Docker network. `MONGODB_PORT=<port>` in `.env` if 27017 is taken.
 - `UNITAO_MONGODB_NETWORK=<network>` in `.env` names the network differently (set the same in the Data Services' `.env`). a network of that name must not exist yet: compose creates it and refuses one it did not create.
 - `.env` holds the password and is ignored by git, like `data/`.

## A user per Data Service

the Data Services can connect as root. to give each one only its own database instead, create a user with the `readWrite` role on it and put that user into the Data Service's config:

```
docker exec -it mongodb mongosh -u root -p --eval 'db.getSiblingDB("admin").createUser({user: "sm-site-shared", pwd: passwordPrompt(), roles: [{role: "readWrite", db: "sm-site-shared"}]})'
```

the user is created in `admin`, where the Data Service authenticates.

## Look inside

```
docker exec -it mongodb mongosh -u root -p
```

## Clean up

```
docker compose down
rm -rf data .env
```

this deletes every database in the instance. `docker compose down` also removes the network, so stop the Data Services that joined it first.
