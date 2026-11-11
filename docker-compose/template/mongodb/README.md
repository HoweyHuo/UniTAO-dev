# mongodb

a MongoDB instance for UniTAO's Data Services, with [mongo-express](https://github.com/mongo-express/mongo-express) as its web UI, as its own compose project. it creates the Docker network `mongo_net`; the Data Services deployed with `unitao_deploy.sh` (see [`../../README.md`](../../README.md)) join it as an external network and reach MongoDB as `mongodb://mongodb:27017`. one instance can serve several Data Services, each with its own database.

skip this if you already run a MongoDB: put its container on a network and point the Data Services at it (`UNITAO_MONGODB_NETWORK`, endpoint, user and password in their configs).

needs Docker Engine with the compose plugin, usable without sudo; on a fresh Ubuntu host `sudo docker-compose/script/docker-deploy.sh` installs it and adds you to group `docker`. where docker.com is unreachable (e.g. in China) it installs from a domestic mirror and sets registry mirrors for Docker Hub by itself.

## Setup

```
docker-compose/script/mongo_deploy.sh ~/infra/mongodb
```

it copies this template into the directory (`docker-compose.yml` and this README; an existing `docker-compose.yml` is kept), writes `.env` (an existing one keeps its values; missing ones are added, e.g. the mongo-express settings in a `.env` from before mongo-express), creates `data/`, starts MongoDB and mongo-express, waits until both are healthy and prints how to connect. it asks for the root password; press enter (or run it non-interactively) to generate one. the other passwords are generated. if a container named `mongodb` or `mongo-express` already runs on the host, it stops: use that MongoDB instead. the compose project name is fixed (`unitao-mongodb`), so it also stops when this template is already deployed from another directory; run it on that directory to update it. run it again on the same directory at any time.

`docker compose` reads `.env` from this directory by itself; variables set in the shell take precedence. `docker compose config` shows the values in effect (including the password).

`docker compose down`, `ps` and `logs` work without `.env` too. `up` without the passwords does not start an open instance: MongoDB exits with `missing … MONGO_INITDB_ROOT_PASSWORD`, and mongo-express (which would otherwise fall back to the login `admin` / `pass`) exits with `a password in .env is empty`. run `mongo_deploy.sh <directory>` to fill them in; it also fills in values left empty (`KEY=`).

| variable | default | |
|---|---|---|
| `MONGODB_ROOT_PASSWORD` | required | password of `root`. only letters, digits and `._~-`: mongo-express puts it into a connection URL as is |
| `UNITAO_UID` / `UNITAO_GID` | `999` | MongoDB runs as this user; `mongo_deploy.sh` sets yours, so the files in `data/` are yours |
| `MONGODB_PORT` | `27017` | port on `127.0.0.1` for tools on the host (`mongosh`, Compass); `mongo_deploy.sh` takes the next free one if 27017 is taken. UniTAO goes through the Docker network |
| `UNITAO_MONGODB_NETWORK` | `mongo_net` | network the Data Services join; set the same in their `.env`. a network of that name must not exist yet: compose creates it and refuses one it did not create |
| `MONGO_EXPRESS_USER` | `admin` | login of the mongo-express web UI |
| `MONGO_EXPRESS_PASSWORD` | required | its password |
| `MONGO_EXPRESS_SECRET` | required | session and cookie secret of mongo-express |
| `MONGO_EXPRESS_PORT` | `8081` | port of the web UI; `mongo_deploy.sh` takes the next free one if 8081 is taken |

 - the root account is created only on the first start, while `data/` is empty. changing `MONGODB_ROOT_PASSWORD` later changes nothing; change it in MongoDB (`db.changeUserPassword`).
 - the databases live in `data/` and survive `docker compose down` and recreating the container.
 - `.env` holds the password (mode 600) and is ignored by git, like `data/`.
 - the compose project is named `unitao-mongodb`, not after the directory, so another compose directory called `mongodb` is not mistaken for this project.

## mongo-express

open `http://<host>:<MONGO_EXPRESS_PORT>` and log in with `MONGO_EXPRESS_USER` / `MONGO_EXPRESS_PASSWORD`. it connects to MongoDB as root, so it can read and change every database, including the Data Services' tables. the port is published on all interfaces so that other machines in the internal network can open it (the Inventory system runs in a protected internal network, see the main README); for this host only, change its `ports` entry to `"127.0.0.1:${MONGO_EXPRESS_PORT:-8081}:8081"`.

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
