FROM busybox:1.37
RUN mkdir /www && echo ok >/www/health
COPY hog.sh /hog.sh
EXPOSE 8080
CMD ["sh", "/hog.sh"]
