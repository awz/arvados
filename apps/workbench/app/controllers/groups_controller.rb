class GroupsController < ApplicationController
  def model_class_for_display
    params[:group_class] || super
  end

  def index
    if params[:group_class]
      @groups = Group.where(group_class: params[:group_class])
    else
      @groups = Group.all
    end
    @group_uuids = @groups.collect &:uuid
    @links_from = Link.where link_class: 'permission', tail_uuid: @group_uuids
    @links_to = Link.where link_class: 'permission', head_uuid: @group_uuids
  end

  def show
    @objects = @object.contents include_linked: true
    super
  end

  def create
    # params[:group_class]=='folder' if we were routed through /folders
    logger.error params.inspect
    if (rsc = params[:group_class])
      params['group'] = (params[rsc] || {}).merge(group_class: rsc)
    end
    super
  end
end
