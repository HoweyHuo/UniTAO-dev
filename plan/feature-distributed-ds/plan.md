现阶段，在UniTAO的设计中，默认Data Service和Inventory Service都存在于一个可以互相接触/访问的网络环境中。

但是，在现实需求中，经常存在在互相隔离的网络环境中需要信息共享的情况。已知的解决方法。

1，建立可互访的环境。

这种环境对系统透明，但是构建和维护都很复杂。而且因为暴露在外界环境中，有一定的安全隐患。

2，通过加密渠道如git共享数据。这种环境复用git的安全机制，而且对更敏感的数据还可以进行二次加密但是数据通过git下载到对面，数据服务需要重新搭建。而且，因为git共享数据的仓库比较集中，每次加载更新都很慢。

所以我的解决方案。

把Communication Channel作为插件。使系统更灵活。

第一阶段先做共享git仓库这个插件。

在UniTao中增加共享Data Service和镜像Data Service，用来加载远端共享过来的schema和数据。

共享Data Service负责正常的读写数据，不同之处是它会把库里的数据变化通过Communication Channel发送给镜像

镜像Data Service只读不写。它的所有改变都来自Communication Channel

Inventory Service依然作为总的查询入口。
