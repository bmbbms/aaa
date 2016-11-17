FROM centos:7
MAINTAINER "891765948@qq.com"
#maintainer
USER root

RUN touch test.txt && echo "abc" >> abc.txt

RUN yum install -y perl-GD

EXPOSE 80 8080 1038


ADD https://www.baidu.com/img/bd_log1.png /opt/
VOLUME ["/usr/local/software","/usr/local"]
