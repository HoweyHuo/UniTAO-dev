# UniTao

## Main Repo Moved To
https://github.com/TuringCompute/UniTAO

## NOTE from Author (Yi Huo):

Apparently, This opensource project is terminated by salesforce.

**All further development will be fully based on my own requirement now.**

**Thus, I decided to fork this project to my own Organization (TuringCompute) for further development**

https://github.com/TuringCompute

**The new repo will be a fork to this repo.**

https://github.com/TuringCompute/UniTAO

all future development will happens to that repo first. 

I will try to sync all future development back here, as long as I have the permission to do so.

## Description

UniTAO was originally created in 2022 by Shai Herzog & Yi Huo as an 
Universal No-Coding Heterogeneous Infrastructure Maintenance & Inventory system that is holistically driven by open/community-developed semantic models/schemas.

It is designed to be self-contained and self-documenting to serve as foundation for human-free automation of heterogeneous computing infrastructure.

The model/schema uses the standard JSON schema format as basis, with additional enhancements. Those can be found in the documentation. 

**schema of schema is defined in path:**
```
lib/Schema/data/schema.json
```
### Demo

#### **Docker**
We can build DataService and Inventory Service into docker container images. so we can run these in a docker environment for demo.

to build all images, run one of the following command based on your target environment.

```
# Powershell script for Windows environment
./docker/buildAll.ps1

# Bash script for Mac or Linux
./docker/buildAll.sh
```

#### **Environment**
after all docker images build successfully. we can bring a set of docker instances up as demo environment.
 - Data Service 01
 - Data Service 01 Admin
 - Data Service 02
 - Data Service 02 Admin
 - Inventory Service
 - Inventory Service Admin
 - Database (DynamoDB / MongoDB)
 - Database Admin (UI interface)

pick a folder and run the command 
**docker compose up -d**
```
# demo environment with DynamoDB
cd ./docker-compose/2data1inv

# demo environment with MongoDB
cd ./docker-compose/2data1invMongo

```

#### **Run**
to run demo:
 - bring up demo environment. wait all instance running stable after multiple restart for data setup
 - login to demo folder
 ```
 ./demo
 ```
 - choose a demo folder and run demo script depending on demo environment.
 ```
 # Windows environment:
 demo.ps1

 # Mac/Linux environment
 demo.sh
 ```


## Components
 
### Data Layer
 - Supported Databases: DynamoDb, MongoDb
 - plugin-able data layer to support multiple types of database
 - Language: GoLang
 - sub folder: ./data


#### REST API Service
 - Language: GOLANG
 - Workspace: all project folders are included in go.work
 - install golang on MacOS
 ```
 brew install go
 ```
 - follow src/README.md to init and run the Inventory Service

## JSON Schema Extensions
the JSON schema of schema to define data format is the key feature of this Data Service.
it give enough flexibility to define JSON data in order to support any coding requirement.

in order to achieve certain feature of Data Service, we have extended JSON schema as following:

### **contentMediaType**
in the standard JSON schema, the original meaning of contentMediaType defines how to parse value of the attached attribute. the following code means value of testAttr is a json string that we can use json library to parse it
```
{
    "name": "testAttr",
    "type": "string",
    "contentMediaType": "json"
}
```

**this implementation does NOT support those standard values.** the only accepted form is the one we introduced:

**contentMediaType = [inventory/{dataType}]**

The following attribute definition means, value of testId is the id of type **test** from **inventory** service
```
{
    "name": "testId",
    "type": "string",
    "contentMediaType": "inventory/test"
}
```

#### **what gets rejected**

every **contentMediaType** on a **string** attribute is parsed, and only the **inventory** prefix is understood. anything else fails the schema preprocess, so the schema cannot even be registered:

```
# the standard values are rejected
"contentMediaType": "json"              -> [contentMediaType]=[json] not supported
"contentMediaType": "application/json"  -> [contentMediaType]=[application] not supported
"contentMediaType": "text/plain"        -> [contentMediaType]=[text] not supported

# a bare type name is rejected too, the prefix is mandatory
"contentMediaType": "actor"             -> [contentMediaType]=[actor] not supported
"contentMediaType": "actor@1.0.0"       -> [contentMediaType]=[actor@1.0.0] not supported
```

note the value is split on the first **/**, so only the part before it is reported - **application/json** shows up as **[application]**.

once the form is correct, the target record is looked up through the Inventory Service, and a reference to a record that does not exist is rejected on write:

```
reference inventory:VmHost with value=[vmhost-test01] does not exists. @path=[VmLink/vm-srv-wireguard-01-link-ext/host]
```

#### **Usage in the demo**

the demo defines a mesh of virtual machine entities spread over two Data Services. it is the best place to see **contentMediaType** in action:

| demo file | what it shows |
|---|---|
| `demo/demo01/data/vmComputeSchema.json` | all schema definitions used below |
| `demo/demo01/steps/06-AcrossDataServiceRef.py` | reference across Data Service, and validation of it |
| `demo/demo01/steps/09-InventoryServiceExplorerData.py` | walking the reference graph by path |
| `demo/demo02/steps/05-CmtIndexAuto.py` | contentMediaType + indexTemplate for auto registered reverse reference |

**1, a reference that crosses the Data Service boundary**

VmLink is defined in DataService01, VmHost is defined in DataService02. a single attribute is enough to point from one into the other:

```
"host": {
    "type": "string",
    "contentMediaType": "inventory/VmHost"
}
```

the value stored is simply the **__id** of the target record, no URL, no DS name:

```
{
    "__id": "vm-srv-wireguard-01-link-ext",
    "__type": "VmLink",
    "__ver": "0.0.2",
    "data": {
        "vm": "vm-srv-wireguard-01",
        "name": "ext",
        "host": "vmhost-test01"
    }
}
```

so the writer of a record never needs to know which Data Service holds the target - the Inventory Service resolves **inventory/VmHost** through its referral table and routes the lookup to the right DS.

that referral table is maintained by the **Inventory Service**, not by the Data Service that owns the type. a type becomes resolvable only after a schema sync has registered it:

```
[sync] referral type[chat-channel-member] to DS[[DataService01]] set
```

the sync runs at Inventory Service startup, when a new Data Service registers itself, and periodically (**sync.intervalSec**, 300 seconds by default). a schema that was just registered on a Data Service is therefore not immediately referenceable - wait for the next sync, or trigger one manually with the admin tool:

```
go run ./tool/InventoryServiceAdmin sync -config <config.json> [-id <ds-id>]
```

**2, the reference is validated on write**

because the target type is declared, DataService validates the value before saving. a reference to an entity that does not exist yet is rejected:

```
# NetworkCard[nic-eth1000-aef] not created yet, rejected by DataService02
PATCH http://localhost:8002/VmHost/vmhost-test01/nic nic-eth1000-aef
    -> 400 reference inventory:NetworkCard with value=[nic-eth1000-aef] does not exists

# create the entity in its own DataService01 first
POST http://localhost:8001 { "__id": "nic-eth1000-aef", "__type": "NetworkCard", ... }

# same PATCH now succeeds
PATCH http://localhost:8002/VmHost/vmhost-test01/nic nic-eth1000-aef
```

note the value is a plain string id, so the validation can only be a [does this record exist] check - the type of the referenced value itself is not inspected here.

**3, array of references**

**contentMediaType** sits on a **string** attribute, so a list of references is simply an array of strings declared under **items**:

```
"virtualMachine": {
    "type": "array",
    "items": {
        "type": "string",
        "contentMediaType": "inventory/VirtualMachine"
    }
}
```

the attribute can also be a nested one, reached through a local **$ref** definition. in demo01 the **VirtualMachine** schema keeps its nic attributes in **definitions/nic**, and the reference inside it is declared exactly the same way:

```
"network": {
    "type": "array",
    "items": {
        "type": "object",
        "$ref": "#/definitions/nic"
    }
},
...
"definitions": {
    "nic": {
        "properties": {
            "link": {
                "type": "string",
                "contentMediaType": "inventory/VmLink"
            }
        }
    }
}
```

**4, walking the reference graph**

after the references are in place, the Inventory Service follows them and lets you walk the graph as if it were one local document. every step of the path below is a **contentMediaType** hop, crossing Data Service borders several times:

```
GET http://localhost:8004/VirtualMachine/vm-srv-wireguard-01/network[eth0]/link/bridge/links[vm-srv-wireguard-02-link-ext]/vm
```

the helper commands of the path engine make the walk explorable, and all of them are demoed in step 09:
 - **?schema** - show the schema of the current path, so you can see the options available
 - **?flat** - show only the current layer instead of the whole referenced complex
 - **[*]** - replace an index with a wildcard to walk all options at that level
 - **?iterator** - list all the options on each leaf, for later filtering

**5, it also drives the reverse reference**

when **contentMediaType** is combined with **indexTemplate**, the same declaration that finds the target type is also used to register the reverse reference on the target record automatically. in step 05 of demo02 adding a **VirtualHardDisk** record makes its id appear under **VmHost/vmhost-test01/virtualHardDisk** without any extra write, and deleting the record removes it again.

#### **Consequences**

declaring a reference is not free - the write time validation puts constraints on the order records can be created in.

**1, the target has to exist first**

since a referencing value is validated on write, the record being pointed at must already be there. a pair that references each other therefore cannot simply be written in either order:

```
write A -> B : rejected if B does not exist yet
write B -> A : rejected if A does not exist yet
```

**2, mutual references**

there is no cycle detection in the code, the deadlock falls straight out of the existence check above combined with **required**. an attribute is **required by default** - it is only optional when it says **"required": false**. so:

 - both directions on **required** attributes = the pair can never be created, there is no valid first write
 - to actually have a mutual reference, make at least one side **"required": false** and write it in two steps: create the record without the attribute, create the other one pointing back at it, then **PATCH** the attribute in
 - the simpler design, and the one used in the demo, is to declare the reference in **one** direction only and keep the other side a plain string

#### **Reason：**
in JSON schema, there is already a key **$ref** that can reference remote schema.

but after close look, we found **$ref** is just a simple include method. it does not include the meaning of parsing and validation. 

so we decide **conentMediaType** are closer to what we really means here. 

that is:
```
we only specify how to find the data that referenced by the value, but we don't really tell you how to use it.
this will seperate the logic of parsing and using of the schema
```

### **indexTemplate**
this attribute is for DataService automation in order to auto fill in reference value in registry attribute of other record.

**Example**

what we want to achieve here is to automatically fill m01 into machines attribute from listA of type machineList when add record machine **id=m01**
```
{
    "machine": [
        {
            "id": "m01"
            "listId": "listA"
        }
    ],
    "machineList": [
        {
            "id": "listA",
            "machines": [
                "m01"
            ]
        }
    ]
}
```


