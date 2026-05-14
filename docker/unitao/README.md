# build docker images

## Image Name:
unitao

## Previous Settings
 - Data Service
 - Inventory Service
 - UI Server
Previous settings will generate 3 seperate images. this is clean on seperate functions.
but it will be hard to manage 3 seperate images repo to maintain and update 

## Current Settings
 - UniTao

with single image, that contain all executibles for Data Service and Inventory, also it contains the web server to be run as UI Server

Also, since we don't have an interface for designing Model yet, so there is no UI Interface feature in there.
